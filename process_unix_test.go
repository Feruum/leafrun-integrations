//go:build !windows

package integrations

import (
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

func hideFixtureProcess(*exec.Cmd) {}
func processRunning(t *testing.T, pid int) bool {
	t.Helper()
	cmd := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid))
	data, err := cmd.Output()
	if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 1 {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	stat := strings.TrimSpace(string(data))
	return stat != "" && !strings.HasPrefix(stat, "Z")
}
