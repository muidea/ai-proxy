package usage

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestMemoryFilterOptionsRangeAndCap(t *testing.T) {
	s := NewMemoryStore()
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	// in-range
	_ = s.Start(context.Background(), StartRecord{EventID: "a1", StartedAt: now, APIKeyID: "codex", Provider: "openai", Model: "gpt-4o"})
	_ = s.Complete(context.Background(), CompleteRecord{EventID: "a1", CompletedAt: now.Add(time.Second), Provider: "openai", Model: "gpt-4o", HTTPStatus: 200, Outcome: "success"})
	_ = s.Start(context.Background(), StartRecord{EventID: "a2", StartedAt: now, APIKeyID: "workorch", Provider: "", Model: ""})
	_ = s.Complete(context.Background(), CompleteRecord{EventID: "a2", CompletedAt: now.Add(time.Second), Provider: "", Model: "", HTTPStatus: 200, Outcome: "success"})
	// out of range
	old := now.AddDate(0, 0, -40)
	_ = s.Start(context.Background(), StartRecord{EventID: "old", StartedAt: old, APIKeyID: "retired", Provider: "old-p", Model: "legacy"})
	_ = s.Complete(context.Background(), CompleteRecord{EventID: "old", CompletedAt: old.Add(time.Second), Provider: "old-p", Model: "legacy", HTTPStatus: 200, Outcome: "success"})

	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
	res, err := s.FilterOptions(context.Background(), FilterOptionsQuery{From: from, To: to})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(res.APIKeyIDs, "codex") || !contains(res.APIKeyIDs, "workorch") {
		t.Fatalf("keys=%v", res.APIKeyIDs)
	}
	if contains(res.APIKeyIDs, "retired") {
		t.Fatalf("out-of-range key leaked: %v", res.APIKeyIDs)
	}
	if !contains(res.Providers, "openai") || contains(res.Providers, "old-p") {
		t.Fatalf("providers=%v", res.Providers)
	}
	if !contains(res.Models, "gpt-4o") || contains(res.Models, "legacy") {
		t.Fatalf("models=%v", res.Models)
	}
	// empty provider/model excluded
	if contains(res.Providers, "") || contains(res.Models, "") {
		t.Fatalf("empty values present")
	}
}

func TestMemoryFilterOptionsTruncation(t *testing.T) {
	s := NewMemoryStore()
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	// MaxFilterOptionValues+5 distinct models
	n := MaxFilterOptionValues + 5
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("e%03d", i)
		model := fmt.Sprintf("model-%03d", i)
		_ = s.Start(context.Background(), StartRecord{EventID: id, StartedAt: now, APIKeyID: "default", Provider: "p", Model: model})
		_ = s.Complete(context.Background(), CompleteRecord{EventID: id, CompletedAt: now.Add(time.Second), Provider: "p", Model: model, HTTPStatus: 200, Outcome: "success"})
	}
	res, err := s.FilterOptions(context.Background(), FilterOptionsQuery{
		From: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Models) != MaxFilterOptionValues {
		t.Fatalf("models len=%d want %d", len(res.Models), MaxFilterOptionValues)
	}
	if !res.Truncated.Models {
		t.Fatalf("expected models truncated")
	}
}

func TestDuckDBFilterOptions(t *testing.T) {
	s := openTestStore(t)
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	_ = s.Start(context.Background(), StartRecord{EventID: "d1", StartedAt: now, APIKeyID: "codex", Provider: "openai", Model: "gpt-4o"})
	_ = s.Complete(context.Background(), CompleteRecord{EventID: "d1", CompletedAt: now.Add(time.Second), Provider: "openai", Model: "gpt-4o", HTTPStatus: 200, Outcome: "success"})
	_ = s.Start(context.Background(), StartRecord{EventID: "d2", StartedAt: now, APIKeyID: "default", Provider: "anthropic", Model: "claude"})
	_ = s.Complete(context.Background(), CompleteRecord{EventID: "d2", CompletedAt: now.Add(time.Second), Provider: "anthropic", Model: "claude", HTTPStatus: 200, Outcome: "success"})

	res, err := s.FilterOptions(context.Background(), FilterOptionsQuery{
		From: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(res.APIKeyIDs, "codex") || !contains(res.APIKeyIDs, "default") {
		t.Fatalf("keys=%v", res.APIKeyIDs)
	}
	if !contains(res.Providers, "openai") || !contains(res.Providers, "anthropic") {
		t.Fatalf("providers=%v", res.Providers)
	}
	if !contains(res.Models, "gpt-4o") || !contains(res.Models, "claude") {
		t.Fatalf("models=%v", res.Models)
	}
	if res.From.IsZero() || res.To.IsZero() {
		t.Fatalf("scope missing: %#v", res)
	}

	// all-time 也必须包含超过常规 366 天窗口的历史维度值。
	old := time.Date(2000, 1, 2, 12, 0, 0, 0, time.UTC)
	if err := s.Start(context.Background(), StartRecord{EventID: "historic", StartedAt: old, APIKeyID: "retired", Provider: "old-p", Model: "legacy"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Complete(context.Background(), CompleteRecord{EventID: "historic", CompletedAt: old.Add(time.Second), Provider: "old-p", Model: "legacy", HTTPStatus: 200, Outcome: "success"}); err != nil {
		t.Fatal(err)
	}
	all, err := s.FilterOptions(context.Background(), FilterOptionsQuery{AllTime: true})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(all.APIKeyIDs, "retired") || !contains(all.Providers, "old-p") || !contains(all.Models, "legacy") {
		t.Fatalf("all-time options missing historic values: %#v", all)
	}
}

func TestResolveFilterOptionsRangeAllTimeMatchesDashboardScope(t *testing.T) {
	from, to, err := ResolveFilterOptionsRange(FilterOptionsQuery{AllTime: true})
	if err != nil {
		t.Fatal(err)
	}
	if !from.Before(to) {
		t.Fatalf("from/to invalid %v %v", from, to)
	}
	if !from.Equal(time.Unix(0, 0).UTC()) {
		t.Fatalf("all-time from=%v want Unix epoch", from)
	}
	if to.Before(time.Now().UTC()) {
		t.Fatalf("all-time to=%v must include now", to)
	}
}

func TestKnownOutcomesIncludesCore(t *testing.T) {
	out := KnownOutcomes()
	for _, want := range []string{"success", "process_interrupted", "upstream_truncated", "upstream_failed", "incomplete"} {
		if !contains(out, want) {
			t.Fatalf("missing %s in %v", want, out)
		}
	}
	if contains(out, "upstream_timeout") {
		t.Fatalf("unexpected obsolete outcome in %v", out)
	}
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
