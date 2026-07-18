package usage

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// MemoryStore 是单测用内存 Store,实现与 DuckDBStore 相同接口语义。
type MemoryStore struct {
	mu      sync.Mutex
	events  map[string]*Event
	closed  atomic.Bool
	healthy atomic.Int32
}

// NewMemoryStore 构造空内存 store。
func NewMemoryStore() *MemoryStore {
	s := &MemoryStore{events: make(map[string]*Event)}
	s.healthy.Store(1)
	return s
}

func (s *MemoryStore) Healthy() bool {
	if s == nil || s.closed.Load() {
		return false
	}
	return s.healthy.Load() == 1
}

func (s *MemoryStore) Start(_ context.Context, rec StartRecord) error {
	if s.closed.Load() {
		return ErrStoreClosed
	}
	if rec.EventID == "" {
		return fmt.Errorf("event_id is required")
	}
	if rec.APIKeyID == "" {
		return fmt.Errorf("api_key_id is required")
	}
	startedAt := rec.StartedAt.UTC()
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.events[rec.EventID]; ok {
		return ErrDuplicateEvent
	}
	s.events[rec.EventID] = &Event{
		EventID:        rec.EventID,
		RoundID:        rec.RoundID,
		StartedAt:      startedAt,
		APIKeyID:       rec.APIKeyID,
		Provider:       rec.Provider,
		Model:          rec.Model,
		Operation:      rec.Operation,
		Route:          rec.Route,
		ClientEndpoint: rec.ClientEndpoint,
		ClientProtocol: rec.ClientProtocol,
		State:          StateStarted,
	}
	s.healthy.Store(1)
	return nil
}

func (s *MemoryStore) Complete(_ context.Context, rec CompleteRecord) error {
	if s.closed.Load() {
		return ErrStoreClosed
	}
	if rec.EventID == "" {
		return fmt.Errorf("event_id is required")
	}
	if rec.InputTokens < 0 || rec.OutputTokens < 0 ||
		rec.CachedInputTokens < 0 || rec.CacheCreationInputTokens < 0 {
		return ErrInvalidTokens
	}
	if rec.HTTPStatus < 100 || rec.HTTPStatus > 599 || strings.TrimSpace(rec.Outcome) == "" {
		return fmt.Errorf("completed usage event requires http_status and outcome")
	}
	total := rec.InputTokens + rec.OutputTokens
	completedAt := rec.CompletedAt.UTC()
	if completedAt.IsZero() {
		completedAt = time.Now().UTC()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.events[rec.EventID]
	if !ok || e.State != StateStarted {
		return ErrEventNotStarted
	}
	if rec.Provider != "" {
		e.Provider = rec.Provider
	}
	if rec.Model != "" {
		e.Model = rec.Model
	}
	e.CompletedAt = &completedAt
	e.UpstreamProtocol = rec.UpstreamProtocol
	e.UpstreamEndpoint = rec.UpstreamEndpoint
	e.ConversionMode = rec.ConversionMode
	e.InputTokens = rec.InputTokens
	e.OutputTokens = rec.OutputTokens
	e.TotalTokens = total
	e.CachedInputTokens = rec.CachedInputTokens
	e.CacheCreationInputTokens = rec.CacheCreationInputTokens
	e.HTTPStatus = rec.HTTPStatus
	e.Outcome = rec.Outcome
	e.ErrorCode = rec.ErrorCode
	e.DurationMS = rec.Duration.Milliseconds()
	e.UpstreamDurationMS = rec.UpstreamDuration.Milliseconds()
	e.Stream = rec.Stream
	e.Estimated = rec.Estimated
	e.State = StateCompleted
	s.healthy.Store(1)
	return nil
}

func (s *MemoryStore) RecoverInterrupted(_ context.Context, at time.Time) (int64, error) {
	if s.closed.Load() {
		return 0, ErrStoreClosed
	}
	if at.IsZero() {
		at = time.Now().UTC()
	} else {
		at = at.UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for _, e := range s.events {
		if e.State != StateStarted {
			continue
		}
		t := at
		e.CompletedAt = &t
		e.HTTPStatus = 500
		e.Outcome = OutcomeProcessInterrupted
		e.ErrorCode = OutcomeProcessInterrupted
		e.State = StateCompleted
		n++
	}
	return n, nil
}

func (s *MemoryStore) Checkpoint(context.Context) error {
	if s.closed.Load() {
		return ErrStoreClosed
	}
	return nil
}

func (s *MemoryStore) Close() error {
	if s == nil {
		return nil
	}
	s.closed.Store(true)
	return nil
}

func (s *MemoryStore) Dashboard(_ context.Context, filter UsageFilter) (Dashboard, error) {
	if s.closed.Load() {
		return Dashboard{}, ErrStoreClosed
	}
	if err := ValidateUsageFilter(&filter); err != nil {
		return Dashboard{}, err
	}
	from, to := filter.From.UTC(), filter.To.UTC()
	if filter.AllTime {
		if from.IsZero() {
			from = time.Unix(0, 0).UTC()
		}
		if to.IsZero() {
			to = time.Now().UTC().Add(24 * time.Hour)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var sum Summary
	dailyMap := map[string]*DailyBucket{}
	keyMap := map[string]*KeySummary{}

	for _, e := range s.events {
		if !matchEvent(e, filter, from, to) {
			continue
		}
		sum.Requests++
		sum.InputTokens += e.InputTokens
		sum.OutputTokens += e.OutputTokens
		sum.TotalTokens += e.TotalTokens
		if e.Outcome == "success" {
			sum.SuccessRequests++
		} else if e.State == StateCompleted {
			sum.FailedRequests++
		}

		day := e.StartedAt.UTC().Format("2006-01-02")
		b, ok := dailyMap[day]
		if !ok {
			b = &DailyBucket{Date: day}
			dailyMap[day] = b
		}
		b.Requests++
		b.InputTokens += e.InputTokens
		b.OutputTokens += e.OutputTokens
		b.TotalTokens += e.TotalTokens

		k, ok := keyMap[e.APIKeyID]
		if !ok {
			k = &KeySummary{APIKeyID: e.APIKeyID}
			keyMap[e.APIKeyID] = k
		}
		k.Requests++
		k.InputTokens += e.InputTokens
		k.OutputTokens += e.OutputTokens
		k.TotalTokens += e.TotalTokens
		if e.Outcome == "success" {
			k.SuccessRequests++
		} else if e.State == StateCompleted {
			k.FailedRequests++
		}
		t := e.StartedAt.UTC()
		if k.LastUsedAt == nil || t.After(*k.LastUsedAt) {
			k.LastUsedAt = &t
		}
	}
	fillSummaryRates(&sum)

	var daily []DailyBucket
	for _, b := range dailyMap {
		daily = append(daily, *b)
	}
	sort.Slice(daily, func(i, j int) bool { return daily[i].Date < daily[j].Date })
	if !filter.AllTime {
		daily = fillMissingDays(from, to, daily)
	}

	var byKey []KeySummary
	for _, k := range keyMap {
		byKey = append(byKey, *k)
	}
	sort.Slice(byKey, func(i, j int) bool {
		if byKey[i].TotalTokens != byKey[j].TotalTokens {
			return byKey[i].TotalTokens > byKey[j].TotalTokens
		}
		return byKey[i].APIKeyID < byKey[j].APIKeyID
	})

	return Dashboard{
		Scope:    ScopeInfo{From: from, To: to, Timezone: "UTC"},
		Summary:  sum,
		Daily:    daily,
		ByAPIKey: byKey,
	}, nil
}

func (s *MemoryStore) Count(ctx context.Context, filter UsageFilter) (int64, error) {
	dashboard, err := s.Dashboard(ctx, filter)
	if err != nil {
		return 0, err
	}
	return dashboard.Summary.Requests, nil
}

func (s *MemoryStore) Events(_ context.Context, filter EventFilter) (EventPage, error) {
	if s.closed.Load() {
		return EventPage{}, ErrStoreClosed
	}
	if err := ValidateEventFilter(&filter); err != nil {
		return EventPage{}, err
	}
	from, to := filter.From.UTC(), filter.To.UTC()
	if filter.AllTime {
		if from.IsZero() {
			from = time.Unix(0, 0).UTC()
		}
		if to.IsZero() {
			to = time.Now().UTC().Add(24 * time.Hour)
		}
	}

	var curAt time.Time
	var curID string
	var hasCursor bool
	if filter.Cursor != "" {
		at, id, err := decodeEventCursor(filter.Cursor)
		if err != nil {
			return EventPage{}, fmt.Errorf("invalid cursor")
		}
		curAt, curID, hasCursor = at, id, true
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var list []*Event
	for _, e := range s.events {
		if !matchEvent(e, filter.UsageFilter, from, to) {
			continue
		}
		if hasCursor {
			if e.StartedAt.After(curAt) {
				continue
			}
			if e.StartedAt.Equal(curAt) && e.EventID >= curID {
				continue
			}
		}
		// 复制一份避免外部修改。
		cp := *e
		list = append(list, &cp)
	}
	sort.Slice(list, func(i, j int) bool {
		if !list[i].StartedAt.Equal(list[j].StartedAt) {
			return list[i].StartedAt.After(list[j].StartedAt)
		}
		return list[i].EventID > list[j].EventID
	})

	limit := filter.PageSize
	page := EventPage{Events: []Event{}}
	if len(list) > limit {
		for _, e := range list[:limit] {
			page.Events = append(page.Events, *e)
		}
		last := page.Events[len(page.Events)-1]
		page.NextCursor = encodeEventCursor(last.StartedAt, last.EventID)
	} else {
		for _, e := range list {
			page.Events = append(page.Events, *e)
		}
	}
	return page, nil
}

func (s *MemoryStore) ExportCSV(_ context.Context, filter UsageFilter, w io.Writer) error {
	if s.closed.Load() {
		return ErrStoreClosed
	}
	if err := ValidateUsageFilter(&filter); err != nil {
		return err
	}
	from, to := filter.From.UTC(), filter.To.UTC()
	if filter.AllTime {
		if from.IsZero() {
			from = time.Unix(0, 0).UTC()
		}
		if to.IsZero() {
			to = time.Now().UTC().Add(24 * time.Hour)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var list []*Event
	for _, e := range s.events {
		if matchEvent(e, filter, from, to) {
			cp := *e
			list = append(list, &cp)
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if !list[i].StartedAt.Equal(list[j].StartedAt) {
			return list[i].StartedAt.Before(list[j].StartedAt)
		}
		return list[i].EventID < list[j].EventID
	})

	cw := csv.NewWriter(w)
	if err := cw.Write(csvExportHeader); err != nil {
		return err
	}
	for _, e := range list {
		completed := ""
		if e.CompletedAt != nil {
			completed = e.CompletedAt.UTC().Format(time.RFC3339Nano)
		}
		httpS := ""
		if e.HTTPStatus != 0 {
			httpS = strconv.Itoa(e.HTTPStatus)
		}
		row := []string{
			e.EventID,
			strconv.FormatInt(e.RoundID, 10),
			e.StartedAt.UTC().Format(time.RFC3339Nano),
			completed,
			e.StartedAt.UTC().Format("2006-01-02"),
			e.APIKeyID,
			e.Provider,
			e.Model,
			e.Operation,
			e.Route,
			e.ClientEndpoint,
			e.ClientProtocol,
			e.UpstreamProtocol,
			e.UpstreamEndpoint,
			e.ConversionMode,
			strconv.FormatInt(e.InputTokens, 10),
			strconv.FormatInt(e.OutputTokens, 10),
			strconv.FormatInt(e.TotalTokens, 10),
			strconv.FormatInt(e.CachedInputTokens, 10),
			strconv.FormatInt(e.CacheCreationInputTokens, 10),
			httpS,
			e.Outcome,
			e.ErrorCode,
			strconv.FormatInt(e.DurationMS, 10),
			strconv.FormatInt(e.UpstreamDurationMS, 10),
			strconv.FormatBool(e.Stream),
			strconv.FormatBool(e.Estimated),
			e.State,
		}
		if err := cw.Write(row); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

func (s *MemoryStore) AllTimeByKey(_ context.Context) (map[string]Summary, error) {
	if s.closed.Load() {
		return nil, ErrStoreClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]Summary)
	for _, e := range s.events {
		sum := out[e.APIKeyID]
		sum.Requests++
		sum.InputTokens += e.InputTokens
		sum.OutputTokens += e.OutputTokens
		sum.TotalTokens += e.TotalTokens
		if e.Outcome == "success" {
			sum.SuccessRequests++
		} else if e.State == StateCompleted {
			sum.FailedRequests++
		}
		out[e.APIKeyID] = sum
	}
	for id, sum := range out {
		fillSummaryRates(&sum)
		out[id] = sum
	}
	return out, nil
}

// FilterOptions 扫描内存事件，按时间窗提取 distinct 值并截断。
func (s *MemoryStore) FilterOptions(_ context.Context, q FilterOptionsQuery) (FilterOptionsResult, error) {
	if s.closed.Load() {
		return FilterOptionsResult{}, ErrStoreClosed
	}
	from, to, err := ResolveFilterOptionsRange(q)
	if err != nil {
		return FilterOptionsResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	keys := make(map[string]struct{})
	providers := make(map[string]struct{})
	models := make(map[string]struct{})
	for _, e := range s.events {
		if e.StartedAt.Before(from) || !e.StartedAt.Before(to) {
			continue
		}
		if e.APIKeyID != "" {
			keys[e.APIKeyID] = struct{}{}
		}
		if e.Provider != "" {
			providers[e.Provider] = struct{}{}
		}
		if e.Model != "" {
			models[e.Model] = struct{}{}
		}
	}
	out := FilterOptionsResult{From: from, To: to}
	out.APIKeyIDs, out.Truncated.APIKeyIDs = capSortedStrings(keys)
	out.Providers, out.Truncated.Providers = capSortedStrings(providers)
	out.Models, out.Truncated.Models = capSortedStrings(models)
	return out, nil
}

func matchEvent(e *Event, f UsageFilter, from, to time.Time) bool {
	if e.StartedAt.Before(from) || !e.StartedAt.Before(to) {
		return false
	}
	if f.APIKeyID != "" && e.APIKeyID != f.APIKeyID {
		return false
	}
	if f.Provider != "" && e.Provider != f.Provider {
		return false
	}
	if f.Model != "" && e.Model != f.Model {
		return false
	}
	if f.Outcome != "" && e.Outcome != f.Outcome {
		return false
	}
	if f.Estimated != nil && e.Estimated != *f.Estimated {
		return false
	}
	return true
}

// 编译期接口断言。
var (
	_ Store = (*DuckDBStore)(nil)
	_ Store = (*MemoryStore)(nil)
)
