package archive

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestRecorderRetainsLatestNumericRoundDirs(t *testing.T) {
	root := t.TempDir()
	for i := 1; i <= 5; i++ {
		if err := os.Mkdir(filepath.Join(root, fmt.Sprintf("%06d", i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}

	recorder, err := NewRecorder(root, 3)
	if err != nil {
		t.Fatal(err)
	}
	assertDirMissing(t, root, "000001")
	assertDirMissing(t, root, "000002")
	assertDirExists(t, root, "000003")
	assertDirExists(t, root, "000004")
	assertDirExists(t, root, "000005")
	assertDirExists(t, root, "notes")

	round, err := recorder.Start()
	if err != nil {
		t.Fatal(err)
	}
	if round.ID != 6 {
		t.Fatalf("round id = %d", round.ID)
	}
	if err := round.WriteMetadata(Metadata{HTTPStatus: 200}); err != nil {
		t.Fatal(err)
	}

	assertDirMissing(t, root, "000003")
	assertDirExists(t, root, "000004")
	assertDirExists(t, root, "000005")
	assertDirExists(t, root, "000006")
	assertDirExists(t, root, "notes")
}

func TestRecorderDoesNotDeleteActiveRounds(t *testing.T) {
	root := t.TempDir()
	recorder, err := NewRecorder(root, 2)
	if err != nil {
		t.Fatal(err)
	}

	active, err := recorder.Start()
	if err != nil {
		t.Fatal(err)
	}
	second, err := recorder.Start()
	if err != nil {
		t.Fatal(err)
	}
	if err := second.WriteMetadata(Metadata{HTTPStatus: 200}); err != nil {
		t.Fatal(err)
	}
	third, err := recorder.Start()
	if err != nil {
		t.Fatal(err)
	}
	if err := third.WriteMetadata(Metadata{HTTPStatus: 200}); err != nil {
		t.Fatal(err)
	}

	assertDirExists(t, root, "000001")
	assertDirExists(t, root, "000002")
	assertDirExists(t, root, "000003")

	if err := active.WriteMetadata(Metadata{HTTPStatus: 200}); err != nil {
		t.Fatal(err)
	}
	assertDirMissing(t, root, "000001")
	assertDirExists(t, root, "000002")
	assertDirExists(t, root, "000003")
}

func assertDirExists(t *testing.T, root, name string) {
	t.Helper()
	info, err := os.Stat(filepath.Join(root, name))
	if err != nil {
		t.Fatalf("expected %s to exist: %v", name, err)
	}
	if !info.IsDir() {
		t.Fatalf("expected %s to be a directory", name)
	}
}

func assertDirMissing(t *testing.T, root, name string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
		t.Fatalf("expected %s to be removed, stat err = %v", name, err)
	}
}

func TestRecorderUsesPrivatePermissions(t *testing.T) {
	root := t.TempDir()
	rec, err := NewRecorder(root, 10)
	if err != nil {
		t.Fatal(err)
	}
	round, err := rec.Start()
	if err != nil {
		t.Fatal(err)
	}
	if err := round.WriteRequest([]byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := round.WriteResponse("response.json", []byte(`{"b":2}`)); err != nil {
		t.Fatal(err)
	}
	if err := round.WriteMetadata(Metadata{HTTPStatus: 200}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(round.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("dir perm = %o, want 700", info.Mode().Perm())
	}
	for _, name := range []string{"request.json", "response.json", "metadata.json"} {
		fi, err := os.Stat(filepath.Join(round.Dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s perm = %o, want 600", name, fi.Mode().Perm())
		}
	}
}

func TestRecorderCanDisableFullContent(t *testing.T) {
	root := t.TempDir()
	rec, err := NewRecorderOptions(root, RecorderOptions{MaxRounds: 10, FullContent: false})
	if err != nil {
		t.Fatal(err)
	}
	round, err := rec.Start()
	if err != nil {
		t.Fatal(err)
	}
	if err := round.WriteRequest([]byte(`{"secret":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := round.WriteResponse("response.json", []byte(`{"out":1}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(round.Dir, "request.json")); !os.IsNotExist(err) {
		t.Fatalf("request.json should be absent when full content disabled, err=%v", err)
	}
}

func TestRecorderMetadataOmitsPathsWhenFullContentDisabled(t *testing.T) {
	root := t.TempDir()
	rec, err := NewRecorderOptions(root, RecorderOptions{MaxRounds: 10, FullContent: false})
	if err != nil {
		t.Fatal(err)
	}
	if rec.FullContent() {
		t.Fatal("FullContent should be false")
	}
	round, err := rec.Start()
	if err != nil {
		t.Fatal(err)
	}
	if round.FullContent() {
		t.Fatal("round FullContent should be false")
	}
}

func TestRoundTracksWrittenFiles(t *testing.T) {
	root := t.TempDir()
	rec, err := NewRecorder(root, 10)
	if err != nil {
		t.Fatal(err)
	}
	round, err := rec.Start()
	if err != nil {
		t.Fatal(err)
	}
	if round.HasFile("request.json") {
		t.Fatal("request.json should not exist yet")
	}
	if err := round.WriteRequest([]byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if !round.HasFile("request.json") {
		t.Fatal("request.json should be tracked")
	}
	if err := round.WriteJSON("upstream_request.json", map[string]string{"u": "1"}); err != nil {
		t.Fatal(err)
	}
	if !round.HasFile("upstream_request.json") {
		t.Fatal("upstream_request.json should be tracked")
	}
}

func TestWriteMetadataReleasesActiveOnWriteFailure(t *testing.T) {
	// metadata 写入失败时仍须释放 active,否则目录永久跳过 retention。
	root := t.TempDir()
	rec, err := NewRecorder(root, 1)
	if err != nil {
		t.Fatal(err)
	}
	done, err := rec.Start()
	if err != nil {
		t.Fatal(err)
	}
	if err := done.WriteMetadata(Metadata{HTTPStatus: 200}); err != nil {
		t.Fatal(err)
	}

	failing, err := rec.Start()
	if err != nil {
		t.Fatal(err)
	}
	// 目录只读,使 metadata.json 写入失败。
	if err := os.Chmod(failing.Dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(failing.Dir, 0o700) })

	if err := failing.WriteMetadata(Metadata{HTTPStatus: 500}); err == nil {
		t.Fatal("expected metadata write error")
	}
	// active 已释放:完成更多 round 后失败目录可被 retention 清理。
	third, err := rec.Start()
	if err != nil {
		t.Fatal(err)
	}
	if err := third.WriteMetadata(Metadata{HTTPStatus: 200}); err != nil {
		t.Fatal(err)
	}
	// maxRounds=1:只保留最新 000003;000001/000002 应删除。
	assertDirMissing(t, root, "000001")
	assertDirMissing(t, root, "000002")
	assertDirExists(t, root, "000003")
	// Abort 幂等
	if err := failing.Abort(); err != nil {
		t.Fatal(err)
	}
}

func TestAbortReleasesActiveWithoutMetadata(t *testing.T) {
	root := t.TempDir()
	rec, err := NewRecorder(root, 1)
	if err != nil {
		t.Fatal(err)
	}
	r1, err := rec.Start()
	if err != nil {
		t.Fatal(err)
	}
	if err := r1.Abort(); err != nil {
		t.Fatal(err)
	}
	// 未写 metadata 也已释放;再完成 2 个 round,r1 目录可被 retention 删除。
	r2, err := rec.Start()
	if err != nil {
		t.Fatal(err)
	}
	if err := r2.WriteMetadata(Metadata{HTTPStatus: 200}); err != nil {
		t.Fatal(err)
	}
	r3, err := rec.Start()
	if err != nil {
		t.Fatal(err)
	}
	if err := r3.WriteMetadata(Metadata{HTTPStatus: 200}); err != nil {
		t.Fatal(err)
	}
	assertDirMissing(t, root, "000001")
	assertDirMissing(t, root, "000002")
	assertDirExists(t, root, "000003")
}
