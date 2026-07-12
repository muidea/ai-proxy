package archive

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Recorder struct {
	root        string
	maxRounds   int
	fullContent bool
	mu          sync.Mutex
	next        int
	active      map[int]struct{}
}

type Round struct {
	ID                 int       `json:"id"`
	Dir                string    `json:"dir"`
	StartedAt          time.Time `json:"started_at"`
	RequestID          string    `json:"request_id,omitempty"`
	StablePrefixHash   string    `json:"stable_prefix_hash,omitempty"`
	RequestFingerprint string    `json:"request_fingerprint,omitempty"`
	recorder           *Recorder
	written            map[string]struct{} // basename -> present
}

func (r *Round) markWritten(name string) {
	if r == nil || name == "" {
		return
	}
	if r.written == nil {
		r.written = map[string]struct{}{}
	}
	r.written[name] = struct{}{}
}

// HasFile 报告本 round 是否成功写入过指定 basename。
func (r *Round) HasFile(name string) bool {
	if r == nil || r.written == nil {
		return false
	}
	_, ok := r.written[name]
	return ok
}

func (r *Round) SetRequestID(id string) {
	if r == nil {
		return
	}
	r.RequestID = id
}

func (r *Round) SetFingerprint(stableHash, fingerprint string) {
	if r == nil {
		return
	}
	r.StablePrefixHash = stableHash
	r.RequestFingerprint = fingerprint
}

type Metadata struct {
	ID                     int       `json:"id"`
	StartedAt              time.Time `json:"started_at"`
	FinishedAt             time.Time `json:"finished_at"`
	RequestID              string    `json:"request_id,omitempty"`
	Provider               string    `json:"provider"`
	Model                  string    `json:"model"`
	StablePrefixHash       string    `json:"stable_prefix_hash,omitempty"`
	RequestFingerprint     string    `json:"request_fingerprint,omitempty"`
	StablePrefixDrift      bool      `json:"stable_prefix_drift,omitempty"`
	StablePrefixDriftCount int       `json:"stable_prefix_drift_count,omitempty"`
	Stream                 bool      `json:"stream"`
	HTTPStatus             int       `json:"http_status"`
	// Outcome 与 CSV/Prometheus 对齐的业务结果枚举。
	Outcome                  string  `json:"outcome,omitempty"`
	DurationMS               int64   `json:"duration_ms"`
	InputTokens              int     `json:"input_tokens"`
	OutputTokens             int     `json:"output_tokens"`
	TotalTokens              int     `json:"total_tokens"`
	CachedInputTokens        int     `json:"cached_input_tokens"`
	CacheCreationInputTokens int     `json:"cache_creation_input_tokens"`
	CacheHitRate             float64 `json:"cache_hit_rate"`
	Estimated                bool    `json:"estimated"`
	RequestPath              string  `json:"request_path,omitempty"`
	RequestMetaPath          string  `json:"request_meta_path,omitempty"`
	UpstreamRequestPath      string  `json:"upstream_request_path,omitempty"`
	UpstreamResponsePath     string  `json:"upstream_response_path,omitempty"`
	ResponsePath             string  `json:"response_path,omitempty"`
	FullResponsePath         string  `json:"full_response_path,omitempty"`
	// FullContentEnabled 标明配置是否启用完整正文归档(不保证磁盘写入一定成功)。
	FullContentEnabled bool   `json:"full_content_enabled"`
	Error              string `json:"error,omitempty"`
}

func NewRecorder(root string, maxRounds ...int) (*Recorder, error) {
	return NewRecorderOptions(root, RecorderOptions{MaxRounds: firstInt(maxRounds, 500), FullContent: true})
}

// RecorderOptions 控制归档行为。
type RecorderOptions struct {
	MaxRounds   int
	FullContent bool
}

// NewRecorderOptions 使用显式选项构造归档器。
func NewRecorderOptions(root string, opts RecorderOptions) (*Recorder, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, nil
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	next, err := nextSequence(root)
	if err != nil {
		return nil, err
	}
	max := opts.MaxRounds
	if max <= 0 {
		max = 500
	}
	recorder := &Recorder{root: root, maxRounds: max, fullContent: opts.FullContent, next: next, active: map[int]struct{}{}}
	if err := recorder.cleanupLocked(); err != nil {
		return nil, err
	}
	return recorder, nil
}

// FullContent 报告是否落盘完整请求/响应正文。
func (r *Recorder) FullContent() bool {
	if r == nil {
		return false
	}
	return r.fullContent
}

// FullContent 报告本 round 所属归档器是否落盘完整正文。
func (r *Round) FullContent() bool {
	if r == nil || r.recorder == nil {
		return true // nil recorder 视为不限制(测试场景)
	}
	return r.recorder.fullContent
}

func firstInt(values []int, fallback int) int {
	if len(values) > 0 {
		return values[0]
	}
	return fallback
}

func (r *Recorder) Start() (*Round, error) {
	if r == nil {
		return nil, nil
	}
	r.mu.Lock()
	id := r.next
	r.next++
	r.active[id] = struct{}{}
	r.mu.Unlock()

	dir := filepath.Join(r.root, fmt.Sprintf("%06d", id))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		r.mu.Lock()
		delete(r.active, id)
		r.mu.Unlock()
		return nil, err
	}
	return &Round{ID: id, Dir: dir, StartedAt: time.Now(), recorder: r, written: map[string]struct{}{}}, nil
}

func (r *Round) WriteRequest(body []byte) error {
	if r == nil {
		return nil
	}
	if r.recorder != nil && !r.recorder.fullContent {
		return nil
	}
	if err := writeJSONOrRaw(filepath.Join(r.Dir, "request.json"), body); err != nil {
		return err
	}
	r.markWritten("request.json")
	return nil
}

func (r *Round) WriteJSON(name string, value any) error {
	if r == nil {
		return nil
	}
	if name == "" {
		name = "metadata.json"
	}
	encoded, err := marshalJSON(value)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(r.Dir, name), encoded, 0o600); err != nil {
		return err
	}
	r.markWritten(name)
	return nil
}

func (r *Round) WriteResponse(name string, body []byte) error {
	if r == nil {
		return nil
	}
	if r.recorder != nil && !r.recorder.fullContent {
		return nil
	}
	if name == "" {
		name = "response.bin"
	}
	if err := os.WriteFile(filepath.Join(r.Dir, name), body, 0o600); err != nil {
		return err
	}
	r.markWritten(name)
	return nil
}

func (r *Round) CreateResponseWriter(name string) (io.WriteCloser, error) {
	if r == nil {
		return nil, nil
	}
	if r.recorder != nil && !r.recorder.fullContent {
		return nopWriteCloser{}, nil
	}
	if name == "" {
		name = "response.bin"
	}
	f, err := os.OpenFile(filepath.Join(r.Dir, name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	r.markWritten(name)
	return f, nil
}

// WriteMetadata 写入 metadata.json 并释放 active 状态。
// 无论 metadata 写入是否成功,都会调用 finish 释放 active,避免 I/O 失败后
// round 永久占用 active map、阻塞 retention 清理。
// 返回值优先保留 metadata 写入错误;finish/cleanup 错误在 metadata 成功时返回。
func (r *Round) WriteMetadata(metadata Metadata) error {
	if r == nil {
		return nil
	}
	metadata.ID = r.ID
	metadata.StartedAt = r.StartedAt
	if metadata.RequestID == "" {
		metadata.RequestID = r.RequestID
	}
	if metadata.StablePrefixHash == "" {
		metadata.StablePrefixHash = r.StablePrefixHash
	}
	if metadata.RequestFingerprint == "" {
		metadata.RequestFingerprint = r.RequestFingerprint
	}
	if metadata.FinishedAt.IsZero() {
		metadata.FinishedAt = time.Now()
	}
	writeErr := r.WriteJSON("metadata.json", metadata)
	// 无论写入成败都释放 active;目录可留给运维排查,但不得永久跳过 retention。
	finishErr := r.finish()
	if writeErr != nil {
		return writeErr
	}
	return finishErr
}

// Abort 在请求中途放弃时释放 active 状态(不写 metadata)。
// 幂等:重复调用安全。用于 handler 在无法完成 WriteMetadata 时保证生命周期闭合。
func (r *Round) Abort() error {
	if r == nil {
		return nil
	}
	return r.finish()
}

func (r *Round) finish() error {
	if r == nil || r.recorder == nil {
		return nil
	}
	return r.recorder.finish(r.ID)
}

func (r *Recorder) finish(id int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.active[id]; !ok {
		// 已释放:跳过重复 retention 扫描(正常路径 WriteMetadata + defer Abort 会二次调用)。
		return nil
	}
	delete(r.active, id)
	return r.cleanupLocked()
}

func (r *Recorder) cleanupLocked() error {
	if r == nil || r.maxRounds <= 0 {
		return nil
	}
	dirs, err := listNumericDirs(r.root)
	if err != nil {
		return err
	}
	removable := make([]archiveDir, 0, len(dirs))
	for _, dir := range dirs {
		if _, ok := r.active[dir.id]; ok {
			continue
		}
		removable = append(removable, dir)
	}
	if len(removable) <= r.maxRounds {
		return nil
	}
	sort.Slice(removable, func(i, j int) bool {
		return removable[i].id < removable[j].id
	})
	for _, dir := range removable[:len(removable)-r.maxRounds] {
		if err := os.RemoveAll(filepath.Join(r.root, dir.name)); err != nil {
			return err
		}
	}
	return nil
}

type archiveDir struct {
	id   int
	name string
}

func listNumericDirs(root string) ([]archiveDir, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	dirs := make([]archiveDir, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		id, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		dirs = append(dirs, archiveDir{id: id, name: entry.Name()})
	}
	return dirs, nil
}

func nextSequence(root string) (int, error) {
	dirs, err := listNumericDirs(root)
	if err != nil {
		return 0, err
	}
	maxID := 0
	for _, dir := range dirs {
		if dir.id > maxID {
			maxID = dir.id
		}
	}
	return maxID + 1, nil
}

func writeJSONOrRaw(path string, body []byte) error {
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return os.WriteFile(path, body, 0o600)
	}
	encoded, err := marshalJSON(value)
	if err != nil {
		return err
	}
	return os.WriteFile(path, encoded, 0o600)
}

func marshalJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }
