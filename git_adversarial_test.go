package integrations

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitObject(t *testing.T, repo, kind string, data []byte) string {
	t.Helper()
	cmd := exec.Command("git", "-c", "core.hooksPath=", "-C", repo, "hash-object", "-w", "-t", kind, "--stdin")
	hideFixtureProcess(cmd)
	cmd.Stdin = bytes.NewReader(data)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("object: %v %s", err, output)
	}
	return strings.TrimSpace(string(output))
}
func gitCommitTree(t *testing.T, repo string, records ...string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", repo, "mktree", "-z")
	hideFixtureProcess(cmd)
	cmd.Stdin = strings.NewReader(strings.Join(records, "\x00") + "\x00")
	data, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mktree: %v %s", err, data)
	}
	tree := strings.TrimSpace(string(data))
	commit := gitObject(t, repo, "commit", []byte("tree "+tree+"\nauthor Fixture <fixture@example.invalid> 1700000000 +0000\ncommitter Fixture <fixture@example.invalid> 1700000000 +0000\n\nfixture\n"))
	fixtureGit(t, repo, "update-ref", "refs/heads/raw", commit)
	return commit
}
func TestGitRejectsUnsupportedTreeModesLFSPathsAndQuotas(t *testing.T) {
	for _, test := range []struct {
		name, mode, path, data string
		want                   error
		limits                 Limits
	}{
		{"symlink", "120000", "link", "target", ErrUnsupportedEntry, Limits{}},
		{"submodule", "160000", "module", "", ErrUnsupportedEntry, Limits{}},
		{"lfs", "100644", "asset", "version https://git-lfs.github.com/spec/v1\noid sha256:" + strings.Repeat("a", 64) + "\nsize 100\n", ErrUnsupportedEntry, Limits{}},
		{"device", "100644", "NUL.txt", "content", ErrUnsafePath, Limits{}},
		{"colon", "100644", "bad:name", "content", ErrUnsafePath, Limits{}},
		{"trailing", "100644", "bad.", "content", ErrUnsafePath, Limits{}},
		{"file-limit", "100644", "main.tex", "12345", ErrLimit, Limits{MaxFileBytes: 4}},
		{"total-limit", "100644", "main.tex", "12345", ErrLimit, Limits{MaxTotalBytes: 4}},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo, head := gitFixture(t)
			oid := head
			kind := "commit"
			if test.mode != "160000" {
				oid = gitObject(t, repo, "blob", []byte(test.data))
				kind = "blob"
			}
			gitCommitTree(t, repo, test.mode+" "+kind+" "+oid+"\t"+test.path)
			temp := t.TempDir()
			_, err := (GitSource{URL: repo, Ref: "raw", Options: GitOptions{TempDir: temp, Limits: test.limits}}).Open(context.Background())
			if !errors.Is(err, test.want) {
				t.Fatalf("wanted %v: %v", test.want, err)
			}
			items, err := os.ReadDir(temp)
			if err != nil || len(items) != 0 {
				t.Fatalf("failure cleanup %v %v", items, err)
			}
		})
	}
	for _, pair := range [][2]string{{"File", "file"}, {"K", "K"}, {"Σ", "ς"}} {
		t.Run("collision-"+pair[0], func(t *testing.T) {
			repo, _ := gitFixture(t)
			oid := gitObject(t, repo, "blob", []byte("x"))
			gitCommitTree(t, repo, "100644 blob "+oid+"\t"+pair[0], "100644 blob "+oid+"\t"+pair[1])
			_, err := (GitSource{URL: repo, Ref: "raw"}).Open(context.Background())
			if !errors.Is(err, ErrUnsafePath) {
				t.Fatalf("collision: %v", err)
			}
		})
	}
	repo, _ := gitFixture(t)
	oid := gitObject(t, repo, "blob", []byte("x"))
	gitCommitTree(t, repo, "100644 blob "+oid+"\ta", "100644 blob "+oid+"\tb")
	_, err := (GitSource{URL: repo, Ref: "raw", Options: GitOptions{Limits: Limits{MaxFiles: 1}}}).Open(context.Background())
	if !errors.Is(err, ErrLimit) {
		t.Fatalf("actual file quota: %v", err)
	}
}
func TestGitRawCommitIgnoresWorkingStateHooksFiltersAndMovingRefs(t *testing.T) {
	repo, oid := gitFixture(t)
	original := filepath.Join(repo, "project space", "日本語", "main.tex")
	if err := os.Remove(original); err != nil {
		t.Fatal(err)
	}
	// A local hook or required checkout filter would fail if acquisition checked out.
	fixtureGit(t, repo, "config", "filter.fail.smudge", "exit 99")
	fixtureGit(t, repo, "config", "filter.fail.required", "true")
	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("*.tex filter=fail\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, repo, "add", ".gitattributes")
	fixtureGit(t, repo, "commit", "-m", "attributes")
	committed := fixtureGit(t, repo, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, ".git", "hooks", "post-checkout"), []byte("#!/bin/sh\nexit 97\n"), 0700); err != nil {
		t.Fatal(err)
	}
	source := GitSource{URL: repo, Ref: committed, SourceRoot: "project space"}
	tree, err := source.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	data, err := fs.ReadFile(tree.FS, "日本語/main.tex")
	if err != nil || string(data) != "raw committed content\n" {
		t.Fatalf("raw commit %q %v", data, err)
	}
	if err := tree.Close(); err != nil {
		t.Fatal(err)
	}
	// Exercise the exact resolve/acquire boundary without timing or fake Git output.
	w, err := newOwnedWorkspace(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer w.close()
	runner, err := newGitRunner(w, GitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := resolveGitRef(context.Background(), runner, repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, repo, "update-ref", "refs/heads/main", oid)
	if accepted != committed {
		t.Fatal("wrong accepted ref")
	}
	if err := acquireGitCommit(context.Background(), runner, repo, accepted); err != nil {
		t.Fatal(err)
	}
	// Removing the whole native working tree after acquisition cannot affect export.
	moved := repo + "-moved"
	if err := os.Rename(repo, moved); err != nil {
		t.Fatal(err)
	}
	defer os.Rename(moved, repo)
	if err := w.root.Mkdir("tree", 0700); err != nil {
		t.Fatal(err)
	}
	root, err := w.root.OpenRoot("tree")
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	limits, _ := (Limits{}).normalized()
	entries, err := exportGitTree(context.Background(), runner, root, accepted, limits)
	if err != nil {
		t.Fatal(err)
	}
	guards := protectWorkingGitFiles(context.Background(), repo, "project space", entries)
	if len(guards) != 0 {
		t.Fatal("missing working tree is not optional")
	}
	f, err := root.Open("project space/日本語/main.tex")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data, err = io.ReadAll(f)
	if err != nil || string(data) != "raw committed content\n" {
		t.Fatalf("accepted moved ref %q %v", data, err)
	}
}
func TestGitBareAndSHA256Commits(t *testing.T) {
	repo, _ := gitFixture(t)
	base := t.TempDir()
	bare := filepath.Join(base, "bare.git")
	fixtureGit(t, base, "clone", "--bare", repo, bare)
	for _, location := range []string{bare} {
		tree, err := (GitSource{URL: location, Ref: "main"}).Open(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(tree.ProtectedPaths) != 0 {
			t.Fatal("bare repository is not an FS mirror")
		}
		if err := tree.Close(); err != nil {
			t.Fatal(err)
		}
	}
	sha := t.TempDir()
	fixtureGit(t, sha, "init", "--object-format=sha256", "-b", "main")
	if err := os.WriteFile(filepath.Join(sha, "main.tex"), []byte("sha256"), 0600); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, sha, "add", ".")
	fixtureGit(t, sha, "commit", "-m", "sha256")
	oid := fixtureGit(t, sha, "rev-parse", "HEAD")
	if len(oid) != 64 {
		t.Fatalf("SHA256 oid %s", oid)
	}
	tree, err := (GitSource{URL: sha, Ref: oid}).Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tree.Origin.Revision != oid {
		t.Fatal("SHA256 revision")
	}
	if err := tree.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestGitCancellationDuringAcquisitionAndStreamingRemovesOwnedTree(t *testing.T) {
	repo, oid := gitFixture(t)
	for _, phase := range []string{"acquire", "stream"} {
		t.Run(phase, func(t *testing.T) {
			temp := t.TempDir()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			w, err := newOwnedWorkspace(ctx, temp)
			if err != nil {
				t.Fatal(err)
			}
			runner, err := newGitRunner(w, GitOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if phase == "acquire" {
				accepted, err := resolveGitRef(ctx, runner, repo, "main")
				if err != nil {
					t.Fatal(err)
				}
				cancel()
				err = acquireGitCommit(ctx, runner, repo, accepted)
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("acquisition cancellation: %v", err)
				}
			} else {
				if err := acquireGitCommit(ctx, runner, repo, oid); err != nil {
					t.Fatal(err)
				}
				if err := w.root.Mkdir("tree", 0700); err != nil {
					t.Fatal(err)
				}
				root, err := w.root.OpenRoot("tree")
				if err != nil {
					t.Fatal(err)
				}
				probe := &probeContext{Context: ctx, probe: func() {
					items, _ := fs.ReadDir(root.FS(), ".")
					if len(items) > 0 {
						cancel()
					}
				}}
				limits, _ := (Limits{}).normalized()
				_, err = exportGitTree(probe, runner, root, oid, limits)
				_ = root.Close()
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("stream cancellation: %v", err)
				}
			}
			if err := w.close(); err != nil {
				t.Fatal(err)
			}
			items, err := os.ReadDir(temp)
			if err != nil || len(items) != 0 {
				t.Fatalf("owned cleanup: %v %v", items, err)
			}
		})
	}
}

func TestGitNestedAndNativeUnicodeDirectoryCollisions(t *testing.T) {
	for _, pair := range [][2]string{{"A", "a"}, {"é", "e\u0301"}} {
		t.Run(pair[0], func(t *testing.T) {
			probe := t.TempDir()
			if err := os.Mkdir(filepath.Join(probe, pair[0]), 0700); err != nil {
				t.Fatal(err)
			}
			err := os.Mkdir(filepath.Join(probe, pair[1]), 0700)
			nativeAlias := errors.Is(err, fs.ErrExist)
			if err != nil && !nativeAlias {
				t.Fatal(err)
			}
			repo, _ := gitFixture(t)
			oid := gitObject(t, repo, "blob", []byte("x"))
			raw, err := hex.DecodeString(oid)
			if err != nil {
				t.Fatal(err)
			}
			subtree := gitObject(t, repo, "tree", append([]byte("100644 main.tex\x00"), raw...))
			gitCommitTree(t, repo, "040000 tree "+subtree+"\t"+pair[0], "040000 tree "+subtree+"\t"+pair[1])
			temp := t.TempDir()
			tree, err := (GitSource{URL: repo, Ref: "raw", Options: GitOptions{TempDir: temp}}).Open(context.Background())
			if foldPath(pair[0]) == foldPath(pair[1]) || nativeAlias {
				if !errors.Is(err, ErrUnsafePath) {
					t.Fatalf("native/logical collision: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			entries, err := fs.ReadDir(tree.FS, ".")
			if err != nil || len(entries) != 2 {
				t.Fatalf("native distinct names: %v %v", entries, err)
			}
			if err := tree.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestGitLargeFileCountAllowanceDoesNotOverflowMetadataLimit(t *testing.T) {
	repo, _ := gitFixture(t)
	tree, err := (GitSource{URL: repo, Ref: "main", Options: GitOptions{Limits: Limits{MaxFiles: 1 << 62}}}).Open(context.Background())
	if err != nil {
		t.Fatalf("large valid count allowance: %v", err)
	}
	if err := tree.Close(); err != nil {
		t.Fatal(err)
	}
}
