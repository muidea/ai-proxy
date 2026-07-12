package stats

import (
	"encoding/csv"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// currentCSVHeader 是当前 usage.csv 列顺序。
var currentCSVHeader = []string{
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
	"outcome",
	"cached_input_tokens",
	"cache_creation_input_tokens",
	"cache_hit_rate",
}

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
	Outcome                  string
	UpstreamDuration         time.Duration
}

type Recorder interface {
	Append(record Record) error
}

// CSVRecorder 将 usage 追加写入 CSV。
// 仅保证单进程安全:schema 轮转依赖本地 Stat+Rename,多进程共享同一文件时可能互相覆盖备份。
type CSVRecorder struct {
	path          string
	mu            sync.Mutex
	schemaChecked bool
}

func NewCSVRecorder(path string) *CSVRecorder {
	return &CSVRecorder{path: path}
}

func (r *CSVRecorder) Append(record Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := r.ensureSchemaLocked(); err != nil {
		return err
	}

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
		if err := writer.Write(currentCSVHeader); err != nil {
			return err
		}
	}
	outcome := record.Outcome
	if outcome == "" {
		if record.HTTPStatus >= 400 {
			outcome = "error"
		} else {
			outcome = "success"
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
		outcome,
		fmt.Sprintf("%d", record.CachedInputTokens),
		fmt.Sprintf("%d", record.CacheCreationInputTokens),
		fmt.Sprintf("%.4f", record.CacheHitRate),
	}); err != nil {
		return err
	}
	writer.Flush()
	return writer.Error()
}

// ensureSchemaLocked 检测旧表头;不匹配时滚动为 usage.csv.bak.<ts> 后写新文件。
// 避免 13 列旧头 + 14 列新数据混写导致严格 CSV 读取失败。
func (r *CSVRecorder) ensureSchemaLocked() error {
	if r.schemaChecked || r.path == "" {
		return nil
	}
	f, err := os.Open(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			r.schemaChecked = true
			return nil
		}
		return err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		r.schemaChecked = true
		return nil
	}

	reader := csv.NewReader(f)
	header, err := reader.Read()
	if err != nil {
		// 无法解析表头:滚动备份,避免继续污染。
		if rotErr := r.rotateLocked("unreadable_header"); rotErr != nil {
			return rotErr
		}
		r.schemaChecked = true
		return nil
	}
	if headersEqual(header, currentCSVHeader) {
		r.schemaChecked = true
		return nil
	}
	if err := r.rotateLocked("schema_mismatch"); err != nil {
		return err
	}
	r.schemaChecked = true
	return nil
}

func (r *CSVRecorder) rotateLocked(reason string) error {
	// 单进程尽力避免覆盖:Stat 后 Rename;仍存在 TOCTOU,多进程请勿共享 usage 文件。
	// 纳秒 + PID;目标存在时跳过(Unix Rename 会覆盖,不能依赖其报错)。
	ts := time.Now().UTC().Format("20060102T150405.000000000Z")
	base := fmt.Sprintf("%s.bak.%s.%d", r.path, ts, os.Getpid())
	candidates := []string{base}
	for i := 1; i <= 100; i++ {
		candidates = append(candidates, fmt.Sprintf("%s.%d", base, i))
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			continue // 已存在,试下一个
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("rotate usage.csv (%s): stat %s: %w", reason, candidate, err)
		}
		if err := os.Rename(r.path, candidate); err != nil {
			return fmt.Errorf("rotate usage.csv (%s): %w", reason, err)
		}
		return nil
	}
	return fmt.Errorf("rotate usage.csv (%s): exhausted backup name candidates", reason)
}

func headersEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if strings.TrimSpace(a[i]) != strings.TrimSpace(b[i]) {
			return false
		}
	}
	return true
}
