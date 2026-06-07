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
