//go:build !windows

package integrations

import (
	"errors"
	"os/exec"
	"syscall"
)

type nativeProcessGroup struct{}

func prepareProcess(cmd *exec.Cmd) (*nativeProcessGroup, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return &nativeProcessGroup{}, nil
}
func (*nativeProcessGroup) started(*exec.Cmd) error { return nil }
func (*nativeProcessGroup) kill(cmd *exec.Cmd) error {
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
func (*nativeProcessGroup) close() error { return nil }
