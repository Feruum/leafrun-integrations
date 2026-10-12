package integrations

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeProtectionRetainsArchiveAndLocalGitIdentity(t *testing.T) {
	repo, _ := gitFixture(t)
	original := filepath.Join(repo, "project space", "日本語", "main.tex")
	alias := filepath.Join(t.TempDir(), "hardlink.tex")
	if err := os.Link(original, alias); err != nil {
		t.Fatal(err)
	}
	aliasInfo, err := os.Stat(alias)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := (GitSource{URL: repo, Ref: "main", SourceRoot: "project space"}).Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	if len(tree.ProtectedPaths) != 0 {
		t.Fatalf("Git directories must not be treated as FS mirrors: %v", tree.ProtectedPaths)
	}
	var found bool
	for _, guard := range tree.NativeProtection {
		if guard.Path == original {
			found = true
			if !os.SameFile(guard.Info, aliasInfo) {
				t.Fatal("original hardlink identity lost")
			}
		}
	}
	if !found {
		t.Fatal("missing selected original file guard")
	}
	if err := os.Remove(original); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(original, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, guard := range tree.NativeProtection {
		if guard.Path == original && !os.SameFile(guard.Info, aliasInfo) {
			t.Fatal("guard reopened replacement")
		}
	}
	if _, err := fs.ReadFile(tree.FS, "日本語/main.tex"); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveNativeProtectionSurvivesReplacement(t *testing.T) {
	original, _ := writeArchive(t, "zip", []archiveRecord{{name: "main.tex", data: []byte("accepted")}})
	alias := filepath.Join(t.TempDir(), "original.zip")
	if err := os.Link(original, alias); err != nil {
		t.Fatal(err)
	}
	aliasHandle, err := os.Open(alias)
	if err != nil {
		t.Fatal(err)
	}
	info, err := aliasHandle.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err := aliasHandle.Close(); err != nil {
		t.Fatal(err)
	}
	tree, err := (ArchiveSource{Path: original}).Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	if len(tree.NativeProtection) != 1 {
		t.Fatalf("missing archive guard: %+v", tree.NativeProtection)
	}
	if err := os.Remove(original); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(original, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(info, tree.NativeProtection[0].Info) {
		t.Fatal("archive guard lost original native identity")
	}
	replacement, err := os.Stat(original)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(replacement, tree.NativeProtection[0].Info) {
		t.Fatal("archive guard points to replacement")
	}
}

func TestGitMetadataGuardsProtectHardlinksWithoutContentMirroring(t *testing.T) {
	repo, _ := gitFixture(t)
	original := filepath.Join(repo, ".git", "config")
	alias := filepath.Join(t.TempDir(), "metadata-alias")
	if err := os.Link(original, alias); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(alias)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	tree, err := (GitSource{URL: repo, Ref: "main", SourceRoot: "project space"}).Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	var guardedDirectory, guardedAlias bool
	for _, guard := range tree.NativeProtection {
		if guard.Path == filepath.Join(repo, ".git") && guard.Info.IsDir() {
			guardedDirectory = true
		}
		if guard.Path == original && os.SameFile(guard.Info, identity) {
			guardedAlias = true
		}
	}
	if !guardedDirectory || !guardedAlias {
		t.Fatalf("metadata directory/file guards missing: directory=%v hardlink=%v", guardedDirectory, guardedAlias)
	}
	fixtureGit(t, repo, "update-ref", "refs/heads/main", tree.Origin.Revision)
	if err := os.Remove(original); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(original, []byte("replacement config"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, guard := range tree.NativeProtection {
		if guard.Path == original && !os.SameFile(guard.Info, identity) {
			t.Fatal("metadata guard lost original identity")
		}
	}
	if len(tree.ProtectedPaths) != 0 {
		t.Fatal("metadata directories are not FS mirrors")
	}
	if err := fs.WalkDir(tree.FS, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name == ".git" || strings.HasPrefix(name, ".git/") {
			t.Fatal("metadata exported")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
