package integrations

import (
	"archive/tar"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTreeWriterDoesNotAdoptUnexpectedNativeDirectory(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.Mkdir("alias", 0700); err != nil {
		t.Fatal(err)
	}
	writer := treeWriter{root: root}
	_, err = writer.writeEntry(context.Background(), preparedEntry{name: "alias/main.typ", size: 2}, strings.NewReader("ok"), 2)
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("unexpected native directory adopted: %v", err)
	}
	if _, err := root.Stat("alias/main.typ"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("wrote through unexpected directory: %v", err)
	}
}

func TestArchiveHandlesNativeUnicodeDirectoryAliases(t *testing.T) {
	probe := t.TempDir()
	composed, decomposed := "é", "e\u0301"
	if err := os.Mkdir(filepath.Join(probe, composed), 0700); err != nil {
		t.Fatal(err)
	}
	err := os.Mkdir(filepath.Join(probe, decomposed), 0700)
	nativeAlias := errors.Is(err, fs.ErrExist)
	if err != nil && !nativeAlias {
		t.Fatal(err)
	}
	for _, kind := range []string{"zip", "tar.gz"} {
		path, _ := writeArchive(t, kind, []archiveRecord{{name: composed + "/a.typ", data: []byte("a"), format: tar.FormatGNU}, {name: decomposed + "/b.typ", data: []byte("b"), format: tar.FormatGNU}})
		if nativeAlias {
			assertArchiveFailure(t, ArchiveSource{Path: path}, fs.ErrExist)
			continue
		}
		tree, err := (ArchiveSource{Path: path}).Open(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		entries, readErr := fs.ReadDir(tree.FS, ".")
		closeErr := tree.Close()
		if readErr != nil || closeErr != nil || len(entries) != 2 {
			t.Fatalf("native unicode directories: %v %v %v", entries, readErr, closeErr)
		}
		seen := map[string]bool{}
		for _, entry := range entries {
			seen[entry.Name()] = true
		}
		if !seen[composed] || !seen[decomposed] {
			t.Fatalf("logical directory names changed: %v", seen)
		}
	}
}
