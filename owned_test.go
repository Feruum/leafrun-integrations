package integrations

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestOwnedWorkspaceRetainsParentAcrossAliasRetarget(t *testing.T) {
	parent := t.TempDir()
	original := filepath.Join(parent, "original")
	replacement := filepath.Join(parent, "replacement")
	alias := filepath.Join(parent, "alias")
	for _, path := range []string{original, replacement} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(original, alias); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	w, err := newOwnedWorkspace(context.Background(), alias)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.close() })
	if err := w.root.Mkdir("tree", 0700); err != nil {
		t.Fatal(err)
	}
	if err := w.root.WriteFile("tree/main.typ", []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	tree, err := w.tree("tree")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(replacement, alias); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(replacement, "sentinel"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := fs.ReadFile(tree, "main.typ")
	if err != nil || string(data) != "original" {
		t.Fatalf("alias redirected FS: %q %v", data, err)
	}
	if err := w.close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(original)
	if err != nil || len(entries) != 0 {
		t.Fatalf("owned original cleanup: %v %v", entries, err)
	}
	if _, err := os.Stat(filepath.Join(replacement, "sentinel")); err != nil {
		t.Fatalf("cleanup followed alias: %v", err)
	}
}

func TestOwnedWorkspacePropagatesRequiredCleanupFailure(t *testing.T) {
	w, err := newOwnedWorkspace(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(w.parent.Name(), w.name)
	t.Cleanup(func() {
		if err := os.RemoveAll(path); err != nil {
			t.Error(err)
		}
	})
	if err := w.parent.Close(); err != nil {
		t.Fatal(err)
	}
	err = w.close()
	if !errors.Is(err, ErrCleanup) || !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("cleanup cause: %v", err)
	}
	if again := w.close(); again != err {
		t.Fatalf("close did not retain failure: %v / %v", err, again)
	}
}

func TestSourceErrorRetainsCategoryAndCause(t *testing.T) {
	cause := &fs.PathError{Op: "read", Path: "original", Err: fs.ErrPermission}
	err := &SourceError{Kind: ErrorArchive, Op: "extract", Path: "x", Err: cause}
	var pathErr *fs.PathError
	if !errors.Is(err, ErrArchive) || !errors.Is(err, fs.ErrPermission) || !errors.As(err, &pathErr) || pathErr != cause {
		t.Fatalf("lost cause: %v", err)
	}
}
