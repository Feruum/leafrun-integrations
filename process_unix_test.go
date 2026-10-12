//go:build !windows

package integrations

import (
	"os/exec"
	"strconv"
	"strings"
)

func hideFixtureProcess(*exec.Cmd) {}

type fixtureProcess struct {
	pid      int
	identity string
}

func captureFixtureProcesses(pids []int) ([]fixtureProcess, error) {
	var observed []fixtureProcess
	for _, pid := range pids {
		identity, _, err := fixtureProcessState(pid)
		if err != nil {
			return nil, err
		}
		observed = append(observed, fixtureProcess{pid: pid, identity: identity})
	}
	return observed, nil
}
func fixtureProcessState(pid int) (string, string, error) {
	cmd := exec.Command("ps", "-o", "lstart=,stat=", "-p", strconv.Itoa(pid))
	data, err := cmd.Output()
	if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 1 {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return "", "", nil
	}
	return strings.Join(fields[:len(fields)-1], " "), fields[len(fields)-1], nil
}
func (p fixtureProcess) running() (bool, error) {
	identity, state, err := fixtureProcessState(p.pid)
	return identity == p.identity && state != "" && !strings.HasPrefix(state, "Z"), err
}
func (fixtureProcess) close() error { return nil }
