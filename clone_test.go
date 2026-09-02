package main

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// A sync that only ever adds leaves the clone growing forever, so syncTree
// must delete what the source dropped -- without touching the caches the
// clone built for itself or the files use-browser writes.
func TestSyncTreePrunes(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()

	write(t, filepath.Join(src, "Preferences"), "kept")
	write(t, filepath.Join(src, "sub", "Bookmarks"), "kept too")

	write(t, filepath.Join(dst, "Preferences"), "stale")
	write(t, filepath.Join(dst, "sub", "Bookmarks"), "stale")
	write(t, filepath.Join(dst, "gone.txt"), "source deleted this")
	write(t, filepath.Join(dst, "old", "deep", "gone.txt"), "and this")
	write(t, filepath.Join(dst, "Cache", "block"), "regenerable, and ours")
	write(t, filepath.Join(dst, "use-browser-port"), "9222")

	st := syncTree(src, dst)

	for _, p := range []string{"Preferences", "sub/Bookmarks"} {
		if !exists(filepath.Join(dst, p)) {
			t.Errorf("%s: missing after sync", p)
		}
	}
	for _, p := range []string{"gone.txt", "old/deep/gone.txt", "old"} {
		if exists(filepath.Join(dst, p)) {
			t.Errorf("%s: still present, prune missed it", p)
		}
	}
	for _, p := range []string{"Cache/block", "use-browser-port"} {
		if !exists(filepath.Join(dst, p)) {
			t.Errorf("%s: pruned, but nothing in the source ever owned it", p)
		}
	}
	if st.Removed != 2 {
		t.Errorf("Removed = %d, want 2", st.Removed)
	}
	if st.Copied != 2 {
		t.Errorf("Copied = %d, want 2", st.Copied)
	}
}

// Cookies and Cookies-wal describe one transaction between them: a fresh
// database beside a stale journal reads as corrupt.
func TestSyncTreeMovesSQLiteGroupTogether(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()

	write(t, filepath.Join(src, "Cookies"), "new database")
	write(t, filepath.Join(src, "Cookies-wal"), "matching journal")
	write(t, filepath.Join(dst, "Cookies"), "old database")
	// Same size and mtime as the source, so on its own it looks up to date.
	write(t, filepath.Join(dst, "Cookies-wal"), "matching journal")
	si, err := os.Stat(filepath.Join(src, "Cookies-wal"))
	if err != nil {
		t.Fatal(err)
	}
	os.Chtimes(filepath.Join(dst, "Cookies-wal"), si.ModTime(), si.ModTime())

	if st := syncTree(src, dst); st.Copied != 2 {
		t.Errorf("Copied = %d, want 2: the journal must travel with its database", st.Copied)
	}
}

// A journal the source has dropped must not survive, or the browser replays
// it against a database that has moved past it.
func TestSyncTreeDropsOrphanSidecar(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()

	write(t, filepath.Join(src, "Cookies"), "checkpointed database")
	write(t, filepath.Join(dst, "Cookies"), "older database")
	write(t, filepath.Join(dst, "Cookies-wal"), "journal the source no longer has")

	syncTree(src, dst)

	if exists(filepath.Join(dst, "Cookies-wal")) {
		t.Error("Cookies-wal: orphan journal survived the sync")
	}
}
