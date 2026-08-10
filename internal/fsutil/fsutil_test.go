package fsutil

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

func TestWriteFileAtomicCreatesParentAndContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "state.json")
	if err := WriteFileAtomic(path, []byte(`{"ok":true}`), FileMode); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"ok":true}` {
		t.Fatalf("content = %q", got)
	}
}

// These files hold assistant output and project paths. They are the user's
// business and nobody else's on a shared machine.
func TestWriteFileAtomicIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "notes.json")
	if err := WriteFileAtomic(path, []byte("x"), FileMode); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := fi.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("file mode %04o is readable by others", mode)
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if mode := di.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("directory mode %04o is readable by others", mode)
	}
}

// A reader must see the old contents or the new ones, never a half-written
// file. This is the property the event spool needed: it wrote the final name
// directly, so Ingest could read a partial file, fail to parse it, and delete
// it as though it had been handled.
func TestWriteFileAtomicNeverExposesAPartialFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	first := []byte("aaaaaaaaaa")
	if err := WriteFileAtomic(path, first, FileMode); err != nil {
		t.Fatal(err)
	}

	big := make([]byte, 1<<20)
	for i := range big {
		big[i] = 'b'
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			got, err := os.ReadFile(path)
			if err != nil {
				continue // mid-rename on some platforms; not a partial read
			}
			// Every observation must be one whole version or the other.
			if len(got) != len(first) && len(got) != len(big) {
				t.Errorf("observed a partial file of %d bytes", len(got))
				return
			}
		}
	}()

	for range 20 {
		if err := WriteFileAtomic(path, big, FileMode); err != nil {
			t.Fatal(err)
		}
		if err := WriteFileAtomic(path, first, FileMode); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
}

// A failed write must not litter the destination directory with temp files.
func TestWriteFileAtomicLeavesNoTempBehind(t *testing.T) {
	dir := t.TempDir()
	if err := WriteFileAtomic(filepath.Join(dir, "a.json"), []byte("x"), FileMode); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "a.json" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("expected just the target file, got %v", names)
	}
}
