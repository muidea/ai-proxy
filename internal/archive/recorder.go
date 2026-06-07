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
	root      string
	maxRounds int
	mu        sync.Mutex
	next      int
	active    map[int]struct{}
}

type Round struct {
	ID        int       `json:"id"`
	Dir       string    `json:"dir"`
	StartedAt time.Time `json:"started_at"`
	recorder  *Recorder
}

type Metadata struct {
	ID                       int       `json:"id"`
	StartedAt                time.Time `json:"started_at"`
	FinishedAt               time.Time `json:"finished_at"`
	Provider                 string    `json:"provider"`
	Model                    string    `json:"model"`
	Stream                   bool      `json:"stream"`
	HTTPStatus               int       `json:"http_status"`
	DurationMS               int64     `json:"duration_ms"`
	InputTokens              int       `json:"input_tokens"`
	OutputTokens             int       `json:"output_tokens"`
	TotalTokens              int       `json:"total_tokens"`
	CachedInputTokens        int       `json:"cached_input_tokens"`
	CacheCreationInputTokens int       `json:"cache_creation_input_tokens"`
	CacheHitRate             float64   `json:"cache_hit_rate"`
	Estimated                bool      `json:"estimated"`
	RequestPath              string    `json:"request_path"`
	RequestMetaPath          string    `json:"request_meta_path,omitempty"`
	UpstreamRequestPath      string    `json:"upstream_request_path,omitempty"`
	UpstreamResponsePath     string    `json:"upstream_response_path,omitempty"`
	ResponsePath             string    `json:"response_path"`
	FullResponsePath         string    `json:"full_response_path,omitempty"`
	Error                    string    `json:"error,omitempty"`
}

func NewRecorder(root string, maxRounds ...int) (*Recorder, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, nil
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	next, err := nextSequence(root)
	if err != nil {
		return nil, err
	}
	max := 500
	if len(maxRounds) > 0 {
		max = maxRounds[0]
	}
	recorder := &Recorder{root: root, maxRounds: max, next: next, active: map[int]struct{}{}}
	if err := recorder.cleanupLocked(); err != nil {
		return nil, err
	}
	return recorder, nil
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
	if err := os.MkdirAll(dir, 0o755); err != nil {
		r.mu.Lock()
		delete(r.active, id)
		r.mu.Unlock()
		return nil, err
	}
	return &Round{ID: id, Dir: dir, StartedAt: time.Now(), recorder: r}, nil
}

func (r *Round) WriteRequest(body []byte) error {
	if r == nil {
		return nil
	}
	return writeJSONOrRaw(filepath.Join(r.Dir, "request.json"), body)
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
	return os.WriteFile(filepath.Join(r.Dir, name), encoded, 0o644)
}

func (r *Round) WriteResponse(name string, body []byte) error {
	if r == nil {
		return nil
	}
	if name == "" {
		name = "response.bin"
	}
	return os.WriteFile(filepath.Join(r.Dir, name), body, 0o644)
}

func (r *Round) CreateResponseWriter(name string) (io.WriteCloser, error) {
	if r == nil {
		return nil, nil
	}
	if name == "" {
		name = "response.bin"
	}
	return os.OpenFile(filepath.Join(r.Dir, name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
}

func (r *Round) WriteMetadata(metadata Metadata) error {
	if r == nil {
		return nil
	}
	metadata.ID = r.ID
	metadata.StartedAt = r.StartedAt
	if metadata.FinishedAt.IsZero() {
		metadata.FinishedAt = time.Now()
	}
	if err := r.WriteJSON("metadata.json", metadata); err != nil {
		return err
	}
	if r.recorder != nil {
		return r.recorder.finish(r.ID)
	}
	return nil
}

func (r *Recorder) finish(id int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
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
		return os.WriteFile(path, body, 0o644)
	}
	encoded, err := marshalJSON(value)
	if err != nil {
		return err
	}
	return os.WriteFile(path, encoded, 0o644)
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
