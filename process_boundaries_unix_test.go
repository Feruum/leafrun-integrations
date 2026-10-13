//go:build linux || darwin

package integrations

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestGitLocalRepositoryTrailingSpace(t *testing.T) {
	repo, _ := gitFixture(t)
	target := filepath.Join(t.TempDir(), "repository ")
	if err := os.Rename(repo, target); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tree, err := (GitSource{URL: target, Ref: "main"}).Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
}

func TestUnixProcessDisappearancePreservesOtherNativeFailures(t *testing.T) {
	for _, failure := range []error{syscall.EBADF, syscall.EACCES, syscall.EIO} {
		if onlyProcessDisappearance(errors.Join(syscall.ESRCH, failure)) {
			t.Fatalf("disappearance must not discard additional native failure %v", failure)
		}
	}
	if !onlyProcessDisappearance(fmt.Errorf("native member vanished: %w", syscall.ESRCH)) {
		t.Fatal("an isolated native disappearance should need no new process descriptor")
	}
}

func TestDetachedOutputHelper(t *testing.T) {
	switch os.Getenv("LEAFRUN_DETACHED_OUTPUT_HELPER") {
	case "leaf":
		os.Stdout.Close()
		os.Stderr.Close()
		time.Sleep(30 * time.Second)
		os.Exit(0)
	case "parent":
		for range 32 {
			cmd := exec.Command(os.Args[0], "-test.run=^TestDetachedOutputHelper$")
			cmd.Env = append(os.Environ(), "LEAFRUN_DETACHED_OUTPUT_HELPER=leaf")
			if err := cmd.Start(); err != nil {
				panic(err)
			}
			fmt.Fprintln(os.Stdout, cmd.Process.Pid)
			_ = cmd.Process.Release()
		}
		os.Exit(0)
	}
}

func TestUnixParentExitObservationKeepsLeaderWaitable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "--version")
	group, err := prepareProcess(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer group.close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = group.kill(cmd)
		_ = cmd.Wait()
	}()
	if err := group.started(cmd); err != nil {
		t.Fatal(err)
	}
	if err := group.wait(cmd); err != nil {
		t.Fatal(err)
	}
	info, err := unixProcess(cmd.Process.Pid)
	if err != nil || info.group != cmd.Process.Pid || !info.exited {
		t.Fatalf("original group leader must remain waitable after exit observation: %#v, %v", info, err)
	}
	if err := group.kill(cmd); err != nil {
		t.Fatal("teardown of an original zombie-only group:", err)
	}
}
