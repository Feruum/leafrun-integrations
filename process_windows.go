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

func (*nativeProcessGroup) wait(cmd *exec.Cmd) error              { return cmd.Wait() }
func (*nativeProcessGroup) reap(_ *exec.Cmd, waitErr error) error { return waitErr }

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
func (g *nativeProcessGroup) kill(*exec.Cmd) (result error) {
	members, threads, captureErr := g.freezeMembers()
	defer func() {
		for _, thread := range threads {
			result = errors.Join(result, windows.CloseHandle(thread))
		}
		for _, member := range members {
			result = errors.Join(result, windows.CloseHandle(member))
		}
	}()
	if err := windows.TerminateJobObject(g.job, 1); err != nil {
		return errors.Join(captureErr, err)
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
			break
		}
		time.Sleep(time.Millisecond)
	}
	// Job accounting reaches zero before every process handle is necessarily
	// signaled. Retained identities prevent PID reuse from changing what we await.
	for _, member := range members {
		status, err := windows.WaitForSingleObject(member, windows.INFINITE)
		if err != nil {
			return errors.Join(captureErr, err)
		}
		if status != windows.WAIT_OBJECT_0 {
			return errors.Join(captureErr, fmt.Errorf("unexpected native process wait status %d", status))
		}
	}
	return captureErr
}

var isProcessInJob = windows.NewLazySystemDLL("kernel32.dll").NewProc("IsProcessInJob")
var suspendNativeThread = windows.NewLazySystemDLL("kernel32.dll").NewProc("SuspendThread")
var nativeThreadOwner = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetProcessIdOfThread")

// Freeze only verified job members, retaining process and thread identities.
// Repeat both enumerations until no new member/thread appears. Once every
// original thread is suspended, no member can create a late descendant between
// the final snapshot and termination. Suspended members are always terminated,
// including when capture fails; no suspended thread is resumed during teardown.
func (g *nativeProcessGroup) freezeMembers() (processes, threads []windows.Handle, result error) {
	owners := make(map[uint32]windows.Handle)
	retainedThreads := make(map[uint32]bool)
	for {
		changed := false
		members, err := g.members()
		if err != nil {
			return processes, threads, err
		}
		for i, handle := range members {
			pid, err := windows.GetProcessId(handle)
			if err != nil {
				_ = windows.CloseHandle(handle)
				for _, other := range members[i+1:] {
					_ = windows.CloseHandle(other)
				}
				return processes, threads, err
			}
			if _, ok := owners[pid]; ok {
				_ = windows.CloseHandle(handle)
			} else {
				owners[pid] = handle
				processes = append(processes, handle)
				changed = true
			}
		}
		if len(owners) == 0 {
			return processes, threads, nil
		}
		snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
		if err != nil {
			return processes, threads, err
		}
		entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
		for err = windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
			if _, ok := owners[entry.OwnerProcessID]; !ok || retainedThreads[entry.ThreadID] {
				continue
			}
			handle, openErr := windows.OpenThread(windows.THREAD_SUSPEND_RESUME|windows.THREAD_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, entry.ThreadID)
			if errors.Is(openErr, windows.ERROR_INVALID_PARAMETER) {
				continue
			}
			if openErr != nil {
				_ = windows.CloseHandle(snapshot)
				return processes, threads, openErr
			}
			pid, _, ownerErr := nativeThreadOwner.Call(uintptr(handle))
			if pid == 0 {
				_ = windows.CloseHandle(handle)
				_ = windows.CloseHandle(snapshot)
				return processes, threads, ownerErr
			}
			if uint32(pid) != entry.OwnerProcessID {
				_ = windows.CloseHandle(handle)
				continue
			}
			previous, _, suspendErr := suspendNativeThread.Call(uintptr(handle))
			if uint32(previous) == 0xffffffff {
				status, waitErr := windows.WaitForSingleObject(handle, 0)
				_ = windows.CloseHandle(handle)
				if waitErr == nil && status == windows.WAIT_OBJECT_0 {
					continue
				}
				_ = windows.CloseHandle(snapshot)
				return processes, threads, errors.Join(suspendErr, waitErr)
			}
			retainedThreads[entry.ThreadID] = true
			threads = append(threads, handle)
			changed = true
		}
		closeErr := windows.CloseHandle(snapshot)
		if !errors.Is(err, windows.ERROR_NO_MORE_FILES) || closeErr != nil {
			return processes, threads, errors.Join(err, closeErr)
		}
		if !changed {
			return processes, threads, nil
		}
	}
}

func (g *nativeProcessGroup) members() (handles []windows.Handle, result error) {
	defer func() {
		if result != nil {
			for _, handle := range handles {
				_ = windows.CloseHandle(handle)
			}
			handles = nil
		}
	}()
	wordSize := int(unsafe.Sizeof(uintptr(0)))
	for capacity := 16; ; {
		buffer := make([]uintptr, 2+capacity)
		err := windows.QueryInformationJobObject(g.job, windows.JobObjectBasicProcessIdList, uintptr(unsafe.Pointer(&buffer[0])), uint32(len(buffer)*wordSize), nil)
		header := (*[2]uint32)(unsafe.Pointer(&buffer[0]))
		if errors.Is(err, windows.ERROR_MORE_DATA) {
			capacity = max(capacity*2, int(header[0])+16)
			continue
		}
		if err != nil {
			return nil, err
		}
		count := int(header[1])
		start := 8 / wordSize
		if count > len(buffer)-start {
			return nil, errors.New("invalid native job process count")
		}
		for _, pid := range buffer[start : start+count] {
			handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
			if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
				continue
			}
			if err != nil {
				return handles, err
			}
			var member int32
			ok, _, callErr := isProcessInJob.Call(uintptr(handle), uintptr(g.job), uintptr(unsafe.Pointer(&member)))
			if ok == 0 {
				_ = windows.CloseHandle(handle)
				return handles, callErr
			}
			if member == 0 {
				_ = windows.CloseHandle(handle)
				continue
			}
			handles = append(handles, handle)
		}
		return handles, nil
	}
}
func (g *nativeProcessGroup) close() error { return windows.CloseHandle(g.job) }
