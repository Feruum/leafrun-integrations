package integrations

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReviewLinkedWorktreeGitfileIdentity(t *testing.T) {
	repo, _ := gitFixture(t)
	linked := filepath.Join(t.TempDir(), "linked")
	fixtureGit(t, repo, "worktree", "add", "-b", "review-linked", linked, "main")
	gitfile := filepath.Join(linked, ".git")
	alias := filepath.Join(t.TempDir(), "gitfile-alias")
	if err := os.Link(gitfile, alias); err != nil {
		t.Fatal(err)
	}
	handle, err := os.Open(alias)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := handle.Stat()
	if err != nil {
		t.Fatal(err)
	}
	handle.Close()
	tree, err := (GitSource{URL: linked, Ref: "review-linked"}).Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	for _, guard := range tree.NativeProtection {
		if os.SameFile(guard.Info, identity) {
			return
		}
	}
	t.Fatalf("ordinary local Git metadata file %s and its hardlink alias have no frozen guard (received %d other guards)", gitfile, len(tree.NativeProtection))
}

func TestGitNativeRewritesPreserveTrustedHTTPSAndFreezeAcceptedTarget(t *testing.T) {
	target, accepted, config := privateHTTPFixtureScoped(t, nil)
	alias := "https://trusted-alias.invalid/repo.git"
	fixtureGit(t, filepath.Dir(config), "config", "--file", config, "url."+target+".insteadOf", alias)
	// Git must apply the original rewrite once, without rewriting its result.
	fixtureGit(t, filepath.Dir(config), "config", "--file", config, "url.http://unsafe.invalid/repo.git.insteadOf", target)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	tree, err := (GitSource{URL: alias, Ref: "main"}).Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if tree.Origin.Location != alias {
		t.Fatalf("requested origin lost: %s", tree.Origin.Location)
	}
	if err := tree.Close(); err != nil {
		t.Fatal(err)
	}
	if accepted.Load() == 0 {
		t.Fatal("native rewritten authentication was not used")
	}
	w, err := newOwnedWorkspace(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer w.close()
	runner, err := newGitRunner(w, GitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	commit, err := resolveGitRef(ctx, runner, alias, "main")
	if err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, filepath.Dir(config), "config", "--file", config, "--unset-all", "url."+target+".insteadOf")
	fixtureGit(t, filepath.Dir(config), "config", "--file", config, "url.https://missing.invalid/repo.git.insteadOf", alias)
	if err := acquireGitCommit(ctx, runner, alias, commit); err != nil {
		t.Fatalf("native rewrite changed accepted transport: %v", err)
	}
}

func TestGitNativeRewritesRejectUnsafeEffectiveTransports(t *testing.T) {
	repo, _ := gitFixture(t)
	for _, target := range []string{"http://example.invalid/repo.git", "ext::unsafe", "https://user:synthetic-secret@example.invalid/repo.git"} {
		t.Run(target, func(t *testing.T) {
			config := filepath.Join(t.TempDir(), "config")
			alias := "https://rewrite-alias.invalid/repo.git"
			fixtureGit(t, repo, "config", "--file", config, "url."+target+".insteadOf", alias)
			t.Setenv("GIT_CONFIG_GLOBAL", config)
			_, err := (GitSource{URL: alias, Ref: "main"}).Open(context.Background())
			if !errors.Is(err, ErrOptions) {
				t.Fatalf("unsafe effective transport: %v", err)
			}
			if strings.Contains(err.Error(), "synthetic-secret") {
				t.Fatalf("rewritten credentials leaked: %v", err)
			}
		})
	}
}

func TestReviewRewrittenHTTPSKeepsTLSVerification(t *testing.T) {
	target, _ := privateHTTPFixture(t)
	config := filepath.Join(t.TempDir(), "review-only-config")
	helper := "!" + quoteSSHArgument(filepath.ToSlash(os.Args[0])) + " -test.run=^TestGitCredentialHelper$ --"
	fixtureGit(t, filepath.Dir(config), "config", "--file", config, "credential.helper", helper)
	fixtureGit(t, filepath.Dir(config), "config", "--file", config, "http."+target+".sslVerify", "false")
	alias := "https://review-alias.invalid/repo.git"
	fixtureGit(t, filepath.Dir(config), "config", "--file", config, "url."+target+".insteadOf", alias)
	t.Setenv("GIT_CONFIG_GLOBAL", config)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tree, err := (GitSource{URL: alias, Ref: "main"}).Open(ctx)
	if err == nil {
		defer tree.Close()
		t.Fatal("native URL rewrite bypassed forced TLS verification and accepted untrusted certificate")
	}
	t.Logf("rejected: %v", err)
}
