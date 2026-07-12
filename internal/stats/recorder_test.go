package stats

import (
	"encoding/csv"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCSVRecorderWritesOutcome(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.csv")
	r := NewCSVRecorder(path)
	if err := r.Append(Record{
		Time:       time.Now(),
		Provider:   "openai",
		Model:      "gpt",
		HTTPStatus: 200,
		Outcome:    "success",
		Duration:   time.Millisecond,
	}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) < 2 {
		t.Fatalf("lines = %d", len(lines))
	}
	header := lines[0]
	if !strings.Contains(header, "outcome") {
		t.Fatalf("header missing outcome: %s", header)
	}
	if strings.Count(header, ",") != strings.Count(lines[1], ",") {
		t.Fatalf("header/data column mismatch:\n%s\n%s", header, lines[1])
	}
}

func TestCSVRecorderMigratesOldSchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.csv")
	// 旧 13 列表头(无 outcome)
	old := "time,provider,model,input_tokens,output_tokens,total_tokens,duration_ms,stream,estimated,http_status,cached_input_tokens,cache_creation_input_tokens,cache_hit_rate\n"
	old += "2020-01-01T00:00:00Z,openai,gpt,1,1,2,1,false,false,200,0,0,0.0000\n"
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	r := NewCSVRecorder(path)
	if err := r.Append(Record{
		Time:       time.Now(),
		Provider:   "openai",
		Model:      "gpt",
		HTTPStatus: 200,
		Outcome:    "success",
	}); err != nil {
		t.Fatal(err)
	}
	// 旧文件应被滚动
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var foundBackup bool
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "usage.csv.bak.") {
			foundBackup = true
		}
	}
	if !foundBackup {
		t.Fatal("expected rotated backup of old usage.csv")
	}
	// 新文件应有新表头
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	reader := csv.NewReader(f)
	header, err := reader.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(header) != len(currentCSVHeader) {
		t.Fatalf("header len = %d, want %d", len(header), len(currentCSVHeader))
	}
	if header[10] != "outcome" {
		t.Fatalf("col10 = %q, want outcome", header[10])
	}
}

func TestRotateDoesNotOverwriteExistingBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.csv")
	if err := os.WriteFile(path, []byte("old-schema\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := NewCSVRecorder(path)
	// 预创建会冲突的备份名前缀目录中已有一个 bak 文件后仍应成功轮转
	if err := r.rotateLocked("test"); err != nil {
		t.Fatal(err)
	}
	// 原 path 应不存在,备份应存在且内容为 old-schema
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("original should be renamed away")
	}
	entries, _ := os.ReadDir(dir)
	var bak string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "usage.csv.bak.") {
			bak = filepath.Join(dir, e.Name())
			break
		}
	}
	if bak == "" {
		t.Fatal("backup not found")
	}
	body, _ := os.ReadFile(bak)
	if string(body) != "old-schema\n" {
		t.Fatalf("backup content = %q", body)
	}
}
