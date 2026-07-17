package usage

import (
	"fmt"
	"strings"
	"time"
	"unicode"
)

const (
	defaultPageSize = 50
	maxPageSize     = 100
	maxFilterLen    = 256
	maxRangeDays    = 366
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
