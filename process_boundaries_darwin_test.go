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
