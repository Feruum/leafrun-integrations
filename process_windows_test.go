package integrations

import (
	"context"
	"errors"
	"golang.org/x/sys/windows"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func hideFixtureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
}
func processRunning(t *testing.T, pid int) bool {
	t.Helper()
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	status, err := windows.WaitForSingleObject(handle, 0)
	if err != nil {
		t.Fatal(err)
	}
	return status == uint32(windows.WAIT_TIMEOUT)
}

func TestWindowsJobTerminationWaitsForAllProcesses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ready := filepath.Join(t.TempDir(), "ready")
	cmd := exec.Command(os.Args[0], "-test.run=^TestNativeProcessHelper$")
	cmd.Env = append(os.Environ(), "LEAFRUN_PROCESS_HELPER=parent-cancel", "LEAFRUN_PROCESS_READY="+ready)
	group, err := prepareProcess(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer group.close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if err := group.started(cmd); err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("descendants not ready")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := group.kill(cmd); err != nil {
		t.Fatal(err)
	}
	var accounting struct {
		User, Kernel, PeriodUser, PeriodKernel int64
		PageFaults, Total, Active, Terminated  uint32
	}
	if err := windows.QueryInformationJobObject(group.job, windows.JobObjectBasicAccountingInformation, uintptr(unsafe.Pointer(&accounting)), uint32(unsafe.Sizeof(accounting)), nil); err != nil {
		t.Fatal(err)
	}
	if accounting.Active != 0 {
		t.Fatalf("job termination returned with %d active processes", accounting.Active)
	}
}
