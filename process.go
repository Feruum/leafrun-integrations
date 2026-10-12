package integrations

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
)

// runNative owns descriptors and the entire native process tree. Readers finish
// before it returns, so its caller may immediately remove operation-owned data.
func runNative(ctx context.Context, cmd *exec.Cmd, consume func(io.Reader) error) (result error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	group, err := prepareProcess(cmd)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, group.close()) }()
	outR, outW, err := os.Pipe()
	if err != nil {
		return err
	}
	defer outR.Close()
	defer outW.Close()
	errR, errW, err := os.Pipe()
	if err != nil {
		return err
	}
	defer errR.Close()
	defer errW.Close()
	cmd.Stdout, cmd.Stderr = outW, errW
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := group.started(cmd); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return err
	}
	stderr := &boundedCapture{limit: 64 << 10}
	errRead := make(chan error, 1)
	go func() { _, err := io.Copy(stderr, errR); errRead <- err }()
	outRead := make(chan error, 1)
	go func() { outRead <- consume(outR) }()
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	var cause, readErr, waitErr error
	var stopErr error
	stop := func() { stopErr = errors.Join(stopErr, group.kill(cmd)) }
	readDone, waitDone := false, false
	for !waitDone {
		select {
		case <-ctx.Done():
			cause = ctx.Err()
			stop()
			waitErr = <-wait
			waitDone = true
		case readErr = <-outRead:
			readDone = true
			outRead = nil
			if readErr != nil {
				stop()
			}
		case waitErr = <-wait:
			waitDone = true
		}
	}
	// A successful parent may leave descendants with inherited pipe handles.
	// Terminate those descendants before awaiting descriptor readers.
	killErr := group.kill(cmd)
	_ = outW.Close()
	_ = errW.Close()
	if !readDone {
		readErr = <-outRead
	}
	stderrErr := <-errRead
	if exitErr := new(exec.ExitError); errors.As(waitErr, &exitErr) {
		exitErr.Stderr = []byte(redactTransport(string(stderr.data)))
	}
	if waitErr != nil && len(stderr.data) != 0 {
		waitErr = &nativeError{cause: waitErr, diagnostic: redactTransport(string(stderr.data))}
	}
	return errors.Join(cause, readErr, waitErr, stderrErr, stopErr, killErr)
}

type nativeError struct {
	cause      error
	diagnostic string
}

func (e *nativeError) Error() string { return e.cause.Error() + ": " + e.diagnostic }
func (e *nativeError) Unwrap() error { return e.cause }

// Capturing diagnostics is bounded even when an external helper writes forever.
type boundedCapture struct {
	data  []byte
	limit int
}

func (b *boundedCapture) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := b.limit - len(b.data); remaining > 0 {
		b.data = append(b.data, p[:min(remaining, len(p))]...)
	}
	return n, nil
}
