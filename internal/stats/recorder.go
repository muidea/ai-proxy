package stats

import (
	"encoding/csv"
	"fmt"
	"os"
	"sync"
	"time"
)

type Record struct {
	Time                     time.Time
	Provider                 string
	Model                    string
	InputTokens              int
	OutputTokens             int
	CachedInputTokens        int
	CacheCreationInputTokens int
	CacheHitRate             float64
	Duration                 time.Duration
	Stream                   bool
	Estimated                bool
	HTTPStatus               int
	UpstreamDuration         time.Duration
}

type Recorder interface {
	Append(record Record) error
}

type CSVRecorder struct {
	path string
	mu   sync.Mutex
}

func NewCSVRecorder(path string) *CSVRecorder {
	return &CSVRecorder{path: path}
}

func (r *CSVRecorder) Append(record Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	file, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return err
	}

	writer := csv.NewWriter(file)
	if info.Size() == 0 {
		if err := writer.Write([]string{
			"time",
			"provider",
			"model",
			"input_tokens",
			"output_tokens",
			"total_tokens",
			"duration_ms",
			"stream",
			"estimated",
			"http_status",
			"cached_input_tokens",
			"cache_creation_input_tokens",
			"cache_hit_rate",
		}); err != nil {
			return err
		}
	}
	total := record.InputTokens + record.OutputTokens
	if err := writer.Write([]string{
		record.Time.Format(time.RFC3339),
		record.Provider,
		record.Model,
		fmt.Sprintf("%d", record.InputTokens),
		fmt.Sprintf("%d", record.OutputTokens),
		fmt.Sprintf("%d", total),
		fmt.Sprintf("%d", record.Duration.Milliseconds()),
		fmt.Sprintf("%t", record.Stream),
		fmt.Sprintf("%t", record.Estimated),
		fmt.Sprintf("%d", record.HTTPStatus),
		fmt.Sprintf("%d", record.CachedInputTokens),
		fmt.Sprintf("%d", record.CacheCreationInputTokens),
		fmt.Sprintf("%.4f", record.CacheHitRate),
	}); err != nil {
		return err
	}
	writer.Flush()
	return writer.Error()
}
