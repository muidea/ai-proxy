package usage

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
)

const (
	defaultPageSize = 50
	maxPageSize     = 100
	maxFilterLen    = 256
	maxRangeDays    = 366

	// MaxFilterOptionValues 是 filter-options 每个维度的返回上限。
	MaxFilterOptionValues = 200
)

// ValidateUsageFilter 规范化并校验时间范围与筛选字段。
func ValidateUsageFilter(f *UsageFilter) error {
	if f == nil {
		return fmt.Errorf("filter is nil")
	}
	if f.AllTime {
		// all-time 不强制 from/to;查询层自行处理。
		return validateFilterStrings(f)
	}
	if f.From.IsZero() && f.To.IsZero() {
		// 默认今日 UTC。
		now := time.Now().UTC()
		start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		f.From = start
		f.To = start.Add(24 * time.Hour)
	}
	if f.From.IsZero() || f.To.IsZero() {
		return fmt.Errorf("from and to are required")
	}
	if !f.From.Before(f.To) {
		return fmt.Errorf("from must be before to")
	}
	if f.To.Sub(f.From) > time.Duration(maxRangeDays)*24*time.Hour {
		return fmt.Errorf("range must not exceed %d days", maxRangeDays)
	}
	return validateFilterStrings(f)
}

func validateFilterStrings(f *UsageFilter) error {
	for _, pair := range []struct {
		name  string
		value string
	}{
		{"api_key_id", f.APIKeyID},
		{"provider", f.Provider},
		{"model", f.Model},
		{"outcome", f.Outcome},
	} {
		v := strings.TrimSpace(pair.value)
		if v == "" {
			continue
		}
		if len(v) > maxFilterLen {
			return fmt.Errorf("%s too long", pair.name)
		}
		for _, r := range v {
			if unicode.IsControl(r) {
				return fmt.Errorf("%s contains control characters", pair.name)
			}
		}
	}
	f.APIKeyID = strings.TrimSpace(f.APIKeyID)
	f.Provider = strings.TrimSpace(f.Provider)
	f.Model = strings.TrimSpace(f.Model)
	f.Outcome = strings.TrimSpace(f.Outcome)
	return nil
}

// ValidateEventFilter 校验明细分页参数。
func ValidateEventFilter(f *EventFilter) error {
	if f == nil {
		return fmt.Errorf("filter is nil")
	}
	if err := ValidateUsageFilter(&f.UsageFilter); err != nil {
		return err
	}
	if f.PageSize <= 0 {
		f.PageSize = defaultPageSize
	}
	if f.PageSize > maxPageSize {
		return fmt.Errorf("page_size must be <= %d", maxPageSize)
	}
	return nil
}

func fillSummaryRates(s *Summary) {
	if s.Requests > 0 {
		s.AvgTokensPerReq = float64(s.TotalTokens) / float64(s.Requests)
		s.SuccessRate = float64(s.SuccessRequests) / float64(s.Requests)
	}
}

// fillMissingDays 保证 [from,to) 内每个 UTC 日期都有桶(缺失补 0)。
func fillMissingDays(from, to time.Time, got []DailyBucket) []DailyBucket {
	if from.IsZero() || to.IsZero() || !from.Before(to) {
		return got
	}
	byDate := make(map[string]DailyBucket, len(got))
	for _, b := range got {
		byDate[b.Date] = b
	}
	var out []DailyBucket
	// 按 UTC 日推进。
	day := time.Date(from.UTC().Year(), from.UTC().Month(), from.UTC().Day(), 0, 0, 0, 0, time.UTC)
	end := to.UTC()
	for day.Before(end) {
		key := day.Format("2006-01-02")
		if b, ok := byDate[key]; ok {
			out = append(out, b)
		} else {
			out = append(out, DailyBucket{Date: key})
		}
		day = day.Add(24 * time.Hour)
	}
	return out
}

// ResolveFilterOptionsRange 规范化 filter-options 扫描窗口。
// all-time 与 Dashboard 保持同一完整历史范围；每个维度的返回数量由
// MaxFilterOptionValues 限制。
func ResolveFilterOptionsRange(q FilterOptionsQuery) (from, to time.Time, err error) {
	if q.AllTime {
		now := time.Now().UTC()
		to = now.Add(24 * time.Hour)
		from = time.Unix(0, 0).UTC()
		return from, to, nil
	}
	from, to = q.From.UTC(), q.To.UTC()
	if from.IsZero() && to.IsZero() {
		now := time.Now().UTC()
		from = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		to = from.Add(24 * time.Hour)
		return from, to, nil
	}
	if from.IsZero() || to.IsZero() {
		return time.Time{}, time.Time{}, fmt.Errorf("from and to are required")
	}
	if !from.Before(to) {
		return time.Time{}, time.Time{}, fmt.Errorf("from must be before to")
	}
	if to.Sub(from) > time.Duration(maxRangeDays)*24*time.Hour {
		return time.Time{}, time.Time{}, fmt.Errorf("range must not exceed %d days", maxRangeDays)
	}
	return from, to, nil
}

// KnownOutcomes 返回 usage outcome 闭集（筛选下拉用，不查库）。
// 与 proxy/stream_result.go 注释及 process_interrupted 恢复常量对齐。
func KnownOutcomes() []string {
	return []string{
		"success",
		"client_canceled",
		"idle_timeout",
		"limit_exceeded",
		"upstream_truncated",
		"upstream_failed",
		"incomplete",
		"client_write",
		"conversion",
		"protocol",
		"error",
		OutcomeProcessInterrupted,
	}
}

// capSortedStrings 将集合排序并截断到 MaxFilterOptionValues，返回截断标志。
func capSortedStrings(set map[string]struct{}) (values []string, truncated bool) {
	values = make([]string, 0, len(set))
	for v := range set {
		if v == "" {
			continue
		}
		values = append(values, v)
	}
	sort.Strings(values)
	if len(values) > MaxFilterOptionValues {
		values = values[:MaxFilterOptionValues]
		truncated = true
	}
	return values, truncated
}
