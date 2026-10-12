package integrations

import (
	"context"
	"golang.org/x/sys/windows"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func hideFixtureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
}

type fixtureProcess struct {
	pid    int
	handle windows.Handle
}

func captureFixtureProcesses(pids []int) ([]fixtureProcess, error) {
	var observed []fixtureProcess
	for _, pid := range pids {
		handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
		if err != nil {
			for _, process := range observed {
				_ = process.close()
			}
			return nil, err
		}
		observed = append(observed, fixtureProcess{pid: pid, handle: handle})
	}
	return observed, nil
}
func (p fixtureProcess) running() (bool, error) {
	status, err := windows.WaitForSingleObject(p.handle, 0)
	return status == uint32(windows.WAIT_TIMEOUT), err
}
func (p fixtureProcess) close() error { return windows.CloseHandle(p.handle) }

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

func TestWindowsNativeGuardsRetainIdentityUnderTempPathAlias(t *testing.T) {
	temp := t.TempDir()
	alias := strings.ToLower(temp)
	t.Setenv("TEMP", alias)
	t.Setenv("TMP", alias)
	t.Run("working-file", TestNativeProtectionRetainsArchiveAndLocalGitIdentity)
	t.Run("metadata", TestGitMetadataGuardsProtectHardlinksWithoutContentMirroring)
}

func TestWindowsQuiescentJobRetainsLateDescendants(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ready := filepath.Join(t.TempDir(), "ready")
	cmd := exec.Command(os.Args[0], "-test.run=^TestNativeProcessHelper$")
	cmd.Env = append(os.Environ(), "LEAFRUN_PROCESS_HELPER=parent-spawn", "LEAFRUN_PROCESS_READY="+ready, "LEAFRUN_PROCESS_ACK=")
	group, err := prepareProcess(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer group.close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = group.kill(cmd); _ = cmd.Wait() }()
	if err := group.started(cmd); err != nil {
		t.Fatal(err)
	}
	for {
		if data, err := os.ReadFile(ready + ".late0"); err == nil && len(strings.Fields(string(data))) == 1 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("late descendant not ready")
		case <-time.After(10 * time.Millisecond):
		}
	}
	processes, threads, err := group.freezeMembers()
	defer func() {
		for _, thread := range threads {
			_ = windows.CloseHandle(thread)
		}
		for _, process := range processes {
			_ = windows.CloseHandle(process)
		}
	}()
	if err != nil {
		t.Fatal(err)
	}
	if len(processes) < 4 {
		t.Fatalf("late original process omitted: retained %d", len(processes))
	}
	stable := make(map[uint32]bool)
	for _, process := range processes {
		pid, err := windows.GetProcessId(process)
		if err != nil {
			t.Fatal(err)
		}
		stable[pid] = true
	}
	// The spawning parent is suspended before termination, so additional members
	// cannot appear after the completed capture, even with delayed cleanup.
	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-time.After(20 * time.Millisecond):
	}
	members, err := group.members()
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range members {
		pid, pidErr := windows.GetProcessId(member)
		_ = windows.CloseHandle(member)
		if pidErr != nil {
			t.Fatal(pidErr)
		}
		if !stable[pid] {
			t.Fatalf("uncaptured late job member %d", pid)
		}
	}
	if err := group.kill(cmd); err != nil {
		t.Fatal(err)
	}
	for _, process := range processes {
		status, err := windows.WaitForSingleObject(process, 0)
		if err != nil {
			t.Fatal(err)
		}
		if status != windows.WAIT_OBJECT_0 {
			t.Fatal("original late member has not exited")
		}
	}
}
