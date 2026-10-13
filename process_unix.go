//go:build linux || darwin

package integrations

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

type nativeProcessGroup struct {
	pid     int
	birth   string
	watches map[string]unixExitWatch
	parent  unixExitWatch
}

type unixProcessInfo struct {
	pid, group int
	birth      string
	exited     bool
}
type unixExitWatch interface {
	exited() (bool, error)
	wait() error
	close() error
}

func prepareProcess(cmd *exec.Cmd) (*nativeProcessGroup, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := checkUnixExitWatch(); err != nil {
		return nil, err
	}
	return &nativeProcessGroup{watches: make(map[string]unixExitWatch)}, nil
}
func (g *nativeProcessGroup) started(cmd *exec.Cmd) error {
	g.pid = cmd.Process.Pid
	info, err := unixProcess(g.pid)
	if err != nil {
		return err
	}
	if info.group != g.pid {
		return errors.New("native child is not in its operation-owned process group")
	}
	g.birth = info.birth
	if err := g.retain(info); err != nil {
		return err
	}
	// Parent completion has its own exit watch. Keep the original leader
	// waitable until group teardown finishes, so its group ID cannot be recycled.
	g.parent, err = newUnixExitWatch(info)
	return err
}

func (g *nativeProcessGroup) wait(*exec.Cmd) error {
	if g.parent == nil {
		return nil // Darwin may already have observed a waitable zombie.
	}
	return g.parent.wait()
}
func (*nativeProcessGroup) reap(cmd *exec.Cmd, observationErr error) error {
	return errors.Join(observationErr, cmd.Wait())
}

func (g *nativeProcessGroup) retain(info unixProcessInfo) error {
	key := fmt.Sprintf("%d:%s", info.pid, info.birth)
	if _, ok := g.watches[key]; ok {
		return nil
	}
	watch, err := newUnixExitWatch(info)
	if onlyProcessDisappearance(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if watch != nil {
		g.watches[key] = watch
	}
	return nil
}

// A vanished member needs no new exit descriptor. Preserve any additional
// native failure, including a failed close joined to a disappearance cause.
func onlyProcessDisappearance(err error) bool {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !onlyProcessDisappearance(cause) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return onlyProcessDisappearance(wrapped.Unwrap())
	}
	return err == syscall.ESRCH || err == syscall.ENOENT || err == os.ErrNotExist
}

func (g *nativeProcessGroup) capture() (bool, error) {
	// A recycled leader PID denotes a different group epoch, which must never
	// receive this operation's signals or become a new exit-wait target.
	leader, err := unixProcess(g.pid)
	if err == nil && leader.birth != g.birth {
		return false, nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ESRCH) {
		return false, err
	}
	members, err := unixGroupMembers(g.pid)
	if err != nil {
		return leader.birth == g.birth, err
	}
	for _, member := range members {
		if err := g.retain(member); err != nil {
			return true, err
		}
	}
	return len(members) > 0, nil
}

func (g *nativeProcessGroup) kill(*exec.Cmd) error {
	for {
		owned, captureErr := g.capture()
		live := false
		var observationErr error
		for _, watch := range g.watches {
			exited, err := watch.exited()
			observationErr = errors.Join(observationErr, err)
			live = live || !exited || err != nil
		}
		var signalErr error
		if owned && (live || captureErr != nil) {
			signalErr = signalUnixGroup(g.pid)
			if onlyProcessDisappearance(signalErr) {
				signalErr = nil
			}
			if signalErr != nil {
				signalErr = fmt.Errorf("terminate operation-owned process group %d: %w", g.pid, signalErr)
			}
		}
		// Observation failures must still await every retained original member
		// before releasing exit descriptors and operation-owned files.
		var waitErr error
		if signalErr == nil {
			for _, watch := range g.watches {
				waitErr = errors.Join(waitErr, watch.wait())
			}
		}
		if err := errors.Join(captureErr, observationErr, signalErr, waitErr); err != nil {
			return err
		}
		owned, captureErr = g.capture()
		if captureErr != nil {
			return captureErr
		}
		live = false
		for _, watch := range g.watches {
			exited, err := watch.exited()
			if err != nil {
				return err
			}
			live = live || !exited
		}
		if !owned || !live {
			return nil
		}
		// Recapture any member born while the original group signal was delivered.
		// Zombies count as exited; reaping belongs to their actual parent.
	}
}
func (g *nativeProcessGroup) close() error {
	var err error
	if g.parent != nil {
		err = g.parent.close()
	}
	for _, watch := range g.watches {
		err = errors.Join(err, watch.close())
	}
	return err
}
