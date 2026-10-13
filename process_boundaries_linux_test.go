//go:build linux

package integrations

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestUnixDetachedDescendantsExitedOnReturn(t *testing.T) {
	if os.Getenv("LEAFRUN_TEST_UNIX_REAPER") != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestUnixDetachedDescendantsExitedOnReturn$")
		cmd.Env = append(os.Environ(), "LEAFRUN_TEST_UNIX_REAPER=1")
		cmd.WaitDelay = time.Second
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("isolated owned-fixture harness: %v\n%s", err, output)
		}
		return
	}
	// The disposable fixture harness adopts and reaps only its own orphan leaves.
	// The production library never changes process-global reaping or signals.
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	defer func() {
		for {
			var status unix.WaitStatus
			_, err := unix.Wait4(-1, &status, 0, nil)
			if err == unix.EINTR {
				continue
			}
			if err == unix.ECHILD {
				return
			}
			if err != nil {
				t.Errorf("drain owned leaves: %v", err)
				return
			}
		}
	}()
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
			pid, e := strconv.Atoi(text)
			if e != nil {
				t.Fatal(e)
			}
			data, e := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
			if os.IsNotExist(e) {
				continue
			}
			if e != nil {
				t.Fatal(e)
			}
			end := strings.LastIndex(string(data), ")")
			state := strings.Fields(string(data)[end+1:])[0]
			if state != "Z" && state != "X" {
				t.Fatalf("attempt %d: descendant %d still state %s after runNative returned", attempt, pid, state)
			}
		}
	}
}
