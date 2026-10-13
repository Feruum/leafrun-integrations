package integrations

import (
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"syscall"
)

type darwinExitWatch struct {
	fd   int
	pid  int
	done bool
}

func signalUnixGroup(group int) error {
	err := unix.Kill(-group, unix.SIGKILL)
	if !errors.Is(err, unix.EPERM) {
		return err
	}
	// Darwin excludes zombies from killpg targets and reports EPERM for an
	// exited-only group. The original leader remains waitable here, preventing
	// group-ID reuse. Accept only a fresh snapshot with no live group members;
	// retained NOTE_EXIT barriers and the late-member scan still run afterward.
	members, lookupErr := unixGroupMembers(group)
	if lookupErr != nil {
		return errors.Join(err, lookupErr)
	}
	for _, member := range members {
		if !member.exited {
			return err
		}
	}
	return nil
}

func nativeExitQueue() (int, error) {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	fd, err := unix.Kqueue()
	if err != nil {
		return fd, fmt.Errorf("create native Darwin exit queue: %w", err)
	}
	unix.CloseOnExec(fd)
	return fd, nil
}
func checkUnixExitWatch() error {
	fd, err := nativeExitQueue()
	if err != nil {
		return err
	}
	return unix.Close(fd)
}
func unixProcess(pid int) (unixProcessInfo, error) {
	// The fixed-record helper reports EIO for a legitimate zero-byte response
	// when a process disappears. The slice helper preserves that exact absence
	// without hiding malformed records or native I/O/permission failures.
	infos, err := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)
	if err != nil {
		return unixProcessInfo{}, fmt.Errorf("snapshot native Darwin process %d: %w", pid, err)
	}
	if len(infos) == 0 {
		return unixProcessInfo{}, unix.ESRCH
	}
	if len(infos) != 1 || int(infos[0].Proc.P_pid) != pid {
		return unixProcessInfo{}, fmt.Errorf("invalid native Darwin process %d snapshot: %w", pid, unix.EIO)
	}
	return darwinProcess(&infos[0]), nil
}
func darwinProcess(info *unix.KinfoProc) unixProcessInfo {
	return unixProcessInfo{pid: int(info.Proc.P_pid), group: int(info.Eproc.Pgid), birth: fmt.Sprintf("%d:%d", info.Proc.P_starttime.Sec, info.Proc.P_starttime.Usec), exited: info.Proc.P_stat == 5}
}
func unixGroupMembers(group int) ([]unixProcessInfo, error) {
	infos, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", group)
	if err != nil {
		return nil, fmt.Errorf("snapshot native Darwin process group %d: %w", group, err)
	}
	members := make([]unixProcessInfo, 0, len(infos))
	for _, info := range infos {
		if int(info.Eproc.Pgid) == group {
			members = append(members, darwinProcess(&info))
		}
	}
	return members, nil
}
func newUnixExitWatch(info unixProcessInfo) (watch unixExitWatch, result error) {
	if info.exited {
		return nil, nil
	}
	fd, err := nativeExitQueue()
	if err != nil {
		return nil, err
	}
	owned := true
	defer func() {
		if owned {
			if err := unix.Close(fd); err != nil {
				result = errors.Join(result, fmt.Errorf("close native Darwin exit queue for process %d: %w", info.pid, err))
			}
		}
	}()
	change := unix.Kevent_t{Ident: uint64(info.pid), Filter: unix.EVFILT_PROC, Flags: unix.EV_ADD | unix.EV_ENABLE | unix.EV_ONESHOT, Fflags: unix.NOTE_EXIT}
	for {
		_, err = unix.Kevent(fd, []unix.Kevent_t{change}, nil, nil)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		attachmentErr := fmt.Errorf("attach native Darwin exit watch to process %d: %w", info.pid, err)
		if errors.Is(err, unix.ESRCH) {
			current, lookupErr := unixProcess(info.pid)
			if lookupErr == nil && current.birth == info.birth && current.group == info.group && current.exited {
				return nil, nil
			}
			if lookupErr != nil {
				return nil, errors.Join(attachmentErr, lookupErr)
			}
			return nil, errors.Join(attachmentErr, errors.New("exit-watch target is not the original exited process identity"))
		}
		return nil, attachmentErr
	}
	current, err := unixProcess(info.pid)
	if err != nil || current.birth != info.birth || current.group != info.group || current.exited {
		if errors.Is(err, unix.ESRCH) || current.exited {
			return nil, nil
		}
		return nil, err
	}
	owned = false
	return &darwinExitWatch{fd: fd, pid: info.pid}, nil
}
func (w *darwinExitWatch) poll(wait bool) (bool, error) {
	if w.done {
		return true, nil
	}
	var timeout *unix.Timespec
	if !wait {
		timeout = &unix.Timespec{}
	}
	for {
		events := make([]unix.Kevent_t, 1)
		n, err := unix.Kevent(w.fd, nil, events, timeout)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("receive native Darwin process %d exit event: %w", w.pid, err)
		}
		if n == 0 {
			return false, nil
		}
		if events[0].Flags&unix.EV_ERROR != 0 {
			return false, fmt.Errorf("native Darwin process %d exit event: %w", w.pid, syscall.Errno(events[0].Data))
		}
		w.done = true
		return true, nil
	}
}
func (w *darwinExitWatch) exited() (bool, error) { return w.poll(false) }
func (w *darwinExitWatch) wait() error           { _, err := w.poll(true); return err }
func (w *darwinExitWatch) close() error          { return unix.Close(w.fd) }
