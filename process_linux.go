package integrations

import (
	"bytes"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"strconv"
	"strings"
)

type linuxExitWatch struct{ fd int }

func signalUnixGroup(group int) error { return unix.Kill(-group, unix.SIGKILL) }

func checkUnixExitWatch() error {
	fd, err := unix.PidfdOpen(os.Getpid(), 0)
	if err != nil {
		return fmt.Errorf("native Linux process exit monitoring requires pidfd_open: %w", err)
	}
	return unix.Close(fd)
}

func unixProcess(pid int) (unixProcessInfo, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return unixProcessInfo{}, err
	}
	end := bytes.LastIndexByte(data, ')')
	if end < 0 {
		return unixProcessInfo{}, errors.New("invalid native process record")
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 20 {
		return unixProcessInfo{}, errors.New("short native process record")
	}
	group, err := strconv.Atoi(fields[2])
	if err != nil {
		return unixProcessInfo{}, err
	}
	return unixProcessInfo{pid: pid, group: group, birth: fields[19], exited: fields[0] == "Z" || fields[0] == "X"}, nil
}

func unixGroupMembers(group int) ([]unixProcessInfo, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var members []unixProcessInfo
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		actual, err := unix.Getpgid(pid)
		if errors.Is(err, unix.ESRCH) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if actual != group {
			continue
		}
		info, err := unixProcess(pid)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ESRCH) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if info.group == group {
			members = append(members, info)
		}
	}
	return members, nil
}

func newUnixExitWatch(info unixProcessInfo) (unixExitWatch, error) {
	fd, err := unix.PidfdOpen(info.pid, 0)
	if err != nil {
		return nil, err
	}
	current, err := unixProcess(info.pid)
	if err != nil || current.birth != info.birth || current.group != info.group {
		_ = unix.Close(fd)
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return &linuxExitWatch{fd: fd}, nil
}

func (w *linuxExitWatch) poll(timeout int) (bool, error) {
	for {
		fds := []unix.PollFd{{Fd: int32(w.fd), Events: unix.POLLIN}}
		_, err := unix.Poll(fds, timeout)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return false, err
		}
		if fds[0].Revents&unix.POLLNVAL != 0 {
			return false, errors.New("native process exit descriptor became invalid")
		}
		return fds[0].Revents&(unix.POLLIN|unix.POLLHUP) != 0, nil
	}
}
func (w *linuxExitWatch) exited() (bool, error) { return w.poll(0) }
func (w *linuxExitWatch) wait() error           { _, err := w.poll(-1); return err }
func (w *linuxExitWatch) close() error          { return unix.Close(w.fd) }
