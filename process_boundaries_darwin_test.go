package integrations

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestUnixDetachedDescendantsExitedOnReturn(t *testing.T) {
	for attempt := range 15 {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := exec.Command(os.Args[0], "-test.run=^TestDetachedOutputHelper$")
		cmd.Env = append(os.Environ(), "LEAFRUN_DETACHED_OUTPUT_HELPER=parent")
		var output strings.Builder
		err := runNative(ctx, cmd, func(r io.Reader) error { _, e := io.Copy(&output, r); return e })
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		for _, text := range strings.Fields(output.String()) {
			pid, err := strconv.Atoi(text)
			if err != nil {
				t.Fatal(err)
			}
			info, err := unixProcess(pid)
			if errors.Is(err, syscall.ESRCH) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			if info.group == cmd.Process.Pid && !info.exited {
				// Only terminate the operation-owned group on an assertion failure.
				// Fixture leaves also expire; launchd reaps their orphan records.
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				t.Fatalf("attempt %d: original descendant %d remains live after return", attempt, pid)
			}
		}
	}
}

func TestDarwinExitWatchBlockedChild(t *testing.T) {
	if os.Getenv("LEAFRUN_DARWIN_EXIT_WATCH_FIXTURE") != "1" {
		return
	}
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		t.Fatal(err)
	}
}

func TestDarwinMissingProcessSnapshotReportsESRCH(t *testing.T) {
	// KERN_PROC_PID performs an exact PID match. Negative IDs cannot identify
	// a process, which makes zero-record absence deterministic without PID reuse.
	_, err := unixProcess(-1)
	if !errors.Is(err, syscall.ESRCH) {
		t.Fatal("snapshot of a nonexistent native process must report ESRCH:", err)
	}
}

func TestDarwinExitWatchAcceptsOriginalWaitableZombie(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDarwinExitWatchBlockedChild$")
	cmd.Env = append(os.Environ(), "LEAFRUN_DARWIN_EXIT_WATCH_FIXTURE=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := false
	defer func() {
		if !reaped {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	live, err := unixProcess(cmd.Process.Pid)
	if err != nil || live.exited || live.group != cmd.Process.Pid {
		t.Fatalf("capture original stdin-blocked live child: %#v, %v", live, err)
	}
	if err := input.Close(); err != nil {
		t.Fatal(err)
	}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		current, err := unixProcess(live.pid)
		if err != nil || current.birth != live.birth || current.group != live.group {
			t.Fatalf("original child identity changed before reaping: %#v, %v", current, err)
		}
		if current.exited {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("owned child did not reach a waitable zombie state:", ctx.Err())
		case <-ticker.C:
		}
	}
	t.Logf("original live snapshot now identifies a waitable zombie: pid=%d group=%d birth=%s", live.pid, live.group, live.birth)
	// Pass the deliberately stale live snapshot: registration must account for
	// the same original identity becoming a zombie before EVFILT_PROC attachment.
	watch, err := newUnixExitWatch(live)
	if watch != nil {
		defer watch.close()
	}
	if err != nil {
		t.Fatal("register exit watch for original already-exited identity:", err)
	}
	if watch != nil {
		exited, err := watch.exited()
		if err != nil || !exited {
			t.Fatalf("original waitable zombie must already satisfy the exit barrier: exited=%v, %v", exited, err)
		}
	}
	err = cmd.Wait()
	reaped = true
	if err != nil {
		t.Fatal("reap successfully completed owned child:", err)
	}
}
