package integrations

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func fixtureGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "core.excludesFile=", "-c", "core.hooksPath=", "-C", dir}, args...)...)
	hideFixtureProcess(cmd)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "no-config"), "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid")
	data, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, data)
	}
	return strings.TrimSpace(string(data))
}
func gitFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	fixtureGit(t, dir, "init", "-b", "main")
	if err := os.MkdirAll(filepath.Join(dir, "project space", "日本語"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "project space", "日本語", "main.tex"), []byte("raw committed content\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, dir, "add", ".")
	fixtureGit(t, dir, "commit", "-m", "fixture")
	return dir, fixtureGit(t, dir, "rev-parse", "HEAD")
}
func TestGitNativeRefsAndIndependentTrees(t *testing.T) {
	repo, oid := gitFixture(t)
	fixtureGit(t, repo, "tag", "light")
	fixtureGit(t, repo, "tag", "-a", "annotated", "-m", "tag")
	for _, ref := range []string{"main", "refs/heads/main", "light", "annotated", oid} {
		t.Run(ref, func(t *testing.T) {
			temp := t.TempDir()
			source := GitSource{URL: repo, Ref: ref, SourceRoot: "project space", Options: GitOptions{TempDir: temp}}
			one, err := source.Open(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			two, err := source.Open(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			data, err := fs.ReadFile(one.FS, "日本語/main.tex")
			if err != nil || string(data) != "raw committed content\n" {
				t.Fatalf("content %q %v", data, err)
			}
			if one.Origin.Kind != "git" || one.Origin.Ref != ref || one.Origin.Revision != oid || one.Origin.SourceRoot != "project space" {
				t.Fatalf("origin %+v", one.Origin)
			}
			reader, err := one.FS.Open("日本語/main.tex")
			if err != nil {
				t.Fatal(err)
			}
			if err := one.Close(); err != nil {
				t.Fatal(err)
			}
			if err := one.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := reader.Read(make([]byte, 1)); !errors.Is(err, fs.ErrClosed) {
				t.Fatalf("reader survived close: %v", err)
			}
			if _, err := fs.ReadFile(two.FS, "日本語/main.tex"); err != nil {
				t.Fatal(err)
			}
			if err := two.Close(); err != nil {
				t.Fatal(err)
			}
			items, err := os.ReadDir(temp)
			if err != nil || len(items) != 0 {
				t.Fatalf("owned removal: %v %v", items, err)
			}
		})
	}
}
