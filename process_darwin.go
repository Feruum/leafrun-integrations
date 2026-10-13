package integrations

import (
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"syscall"
)

type darwinExitWatch struct {
	fd   int
	done bool
}

func nativeExitQueue() (int, error) {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	fd, err := unix.Kqueue()
	if err == nil {
		unix.CloseOnExec(fd)
	}
	return fd, err
}
func checkUnixExitWatch() error {
	fd, err := nativeExitQueue()
	if err != nil {
		return err
	}
	return unix.Close(fd)
}
func unixProcess(pid int) (unixProcessInfo, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return unixProcessInfo{}, err
	}
	if info.Proc.P_pid == 0 {
		return unixProcessInfo{}, unix.ESRCH
	}
	return darwinProcess(info), nil
}
func darwinProcess(info *unix.KinfoProc) unixProcessInfo {
	return unixProcessInfo{pid: int(info.Proc.P_pid), group: int(info.Eproc.Pgid), birth: fmt.Sprintf("%d:%d", info.Proc.P_starttime.Sec, info.Proc.P_starttime.Usec), exited: info.Proc.P_stat == 5}
}
func unixGroupMembers(group int) ([]unixProcessInfo, error) {
	infos, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", group)
	if err != nil {
		return nil, err
	}
	members := make([]unixProcessInfo, 0, len(infos))
	for _, info := range infos {
		if int(info.Eproc.Pgid) == group {
			members = append(members, darwinProcess(&info))
		}
	}
	return members, nil
}
func newUnixExitWatch(info unixProcessInfo) (unixExitWatch, error) {
	if info.exited {
		return nil, nil
	}
	fd, err := nativeExitQueue()
	if err != nil {
		return nil, err
	}
	change := unix.Kevent_t{Ident: uint64(info.pid), Filter: unix.EVFILT_PROC, Flags: unix.EV_ADD | unix.EV_ENABLE | unix.EV_ONESHOT, Fflags: unix.NOTE_EXIT}
	for {
		_, err = unix.Kevent(fd, []unix.Kevent_t{change}, nil, nil)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	current, err := unixProcess(info.pid)
	if err != nil || current.birth != info.birth || current.group != info.group || current.exited {
		_ = unix.Close(fd)
		if errors.Is(err, unix.ESRCH) || current.exited {
			return nil, nil
		}
		return nil, err
	}
	return &darwinExitWatch{fd: fd}, nil
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
			return false, err
		}
		if n == 0 {
			return false, nil
		}
		if events[0].Flags&unix.EV_ERROR != 0 {
			return false, syscall.Errno(events[0].Data)
		}
		w.done = true
		return true, nil
	}
}
func (w *darwinExitWatch) exited() (bool, error) { return w.poll(false) }
func (w *darwinExitWatch) wait() error           { _, err := w.poll(true); return err }
func (w *darwinExitWatch) close() error          { return unix.Close(w.fd) }
