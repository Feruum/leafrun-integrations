package integrations

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

type nativeProcessGroup struct{ job windows.Handle }

func prepareProcess(cmd *exec.Cmd) (*nativeProcessGroup, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	_, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits)))
	if err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_SUSPENDED | windows.CREATE_NO_WINDOW}
	return &nativeProcessGroup{job: job}, nil
}

func (g *nativeProcessGroup) started(cmd *exec.Cmd) error {
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(cmd.Process.Pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(process)
	if err = windows.AssignProcessToJobObject(g.job, process); err != nil {
		return err
	}
	// Go closes CreateProcess's primary thread handle. The suspended child has
	// executed no code and has exactly one thread; find only that child's thread
	// after assignment, and resume it while the process handle prevents PID reuse.
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	var thread windows.Handle
	defer func() {
		if thread != 0 {
			_ = windows.CloseHandle(thread)
		}
	}()
	for err = windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID != uint32(cmd.Process.Pid) {
			continue
		}
		if thread != 0 {
			return errors.New("suspended child unexpectedly has multiple threads")
		}
		thread, err = windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if err != nil {
			return err
		}
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return err
	}
	if thread == 0 {
		return errors.New("suspended child primary thread not found")
	}
	previous, err := windows.ResumeThread(thread)
	if err != nil {
		return err
	}
	if previous != 1 {
		return fmt.Errorf("unexpected primary thread suspend count %d", previous)
	}
	return nil
}
func (g *nativeProcessGroup) kill(*exec.Cmd) error {
	if err := windows.TerminateJobObject(g.job, 1); err != nil {
		return err
	}
	// TerminateJobObject has TerminateProcess semantics. Do not infer completion
	// from a pipe closing: descendants may close stdout before releasing files.
	for {
		var accounting struct {
			User, Kernel, PeriodUser, PeriodKernel int64
			PageFaults, Total, Active, Terminated  uint32
		}
		if err := windows.QueryInformationJobObject(g.job, windows.JobObjectBasicAccountingInformation, uintptr(unsafe.Pointer(&accounting)), uint32(unsafe.Sizeof(accounting)), nil); err != nil {
			return err
		}
		if accounting.Active == 0 {
			return nil
		}
		time.Sleep(time.Millisecond)
	}
}
func (g *nativeProcessGroup) close() error { return windows.CloseHandle(g.job) }
