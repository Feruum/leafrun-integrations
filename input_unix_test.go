//go:build unix

package integrations

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Probes run in a bounded child process so a regression in native acquisition
// cannot leave a blocked goroutine in the test runner or retain its fixtures.
func TestArchiveUnixInputAcquisition(t *testing.T) {
	for _, mode := range []string{"fifo", "opened_fifo_replaced", "opened_regular_replaced", "cancel_after_open"} {
		t.Run(mode, func(t *testing.T) {
			input, raw := writeArchive(t, "zip", []archiveRecord{{name: "main.typ", data: []byte("ok")}})
			replacement := filepath.Join(t.TempDir(), "replacement.zip")
			if err := os.WriteFile(replacement, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if mode == "fifo" || mode == "opened_fifo_replaced" {
				if err := os.Remove(input); err != nil {
					t.Fatal(err)
				}
				if err := syscall.Mkfifo(input, 0600); err != nil {
					t.Fatal(err)
				}
			}
			parent, signals := t.TempDir(), t.TempDir()
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestArchiveUnixInputProbe$", "-test.count=1")
			command.WaitDelay = time.Second
			command.Env = append(os.Environ(),
				"LEAFRUN_ARCHIVE_INPUT_PROBE="+mode,
				"LEAFRUN_ARCHIVE_INPUT_PATH="+input,
				"LEAFRUN_ARCHIVE_INPUT_REPLACEMENT="+replacement,
				"LEAFRUN_ARCHIVE_INPUT_PARENT="+parent,
				"LEAFRUN_ARCHIVE_INPUT_SIGNALS="+signals)
			var output bytes.Buffer
			command.Stdout, command.Stderr = &output, &output
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			var began time.Time
			var unblock *os.File
			var probeErr error
			released := false
			ticker := time.NewTicker(10 * time.Millisecond)
			for ctx.Err() == nil {
				if _, err := os.Stat(filepath.Join(signals, "finished")); err == nil {
					break
				}
				if began.IsZero() {
					if _, err := os.Stat(filepath.Join(signals, "started")); err == nil {
						began = time.Now()
					}
				} else if time.Since(began) > 500*time.Millisecond && !released {
					// Unblock a defective blocking open before collecting its result.
					// The process deadline remains a second bound if the probe fails.
					released = true
					unblock, probeErr = os.OpenFile(input, os.O_RDWR|syscall.O_NONBLOCK, 0)
				}
				select {
				case <-ticker.C:
				case <-ctx.Done():
				}
			}
			ticker.Stop()
			waitErr := command.Wait()
			if unblock != nil {
				if err := unblock.Close(); err != nil {
					t.Error(err)
				}
			}
			entries, readErr := os.ReadDir(parent)
			if readErr != nil || len(entries) != 0 {
				t.Errorf("owned acquisition resources retained: %v %v", entries, readErr)
			}
			if _, err := os.Lstat(input); err != nil {
				t.Errorf("external input removed: %v", err)
			}
			if released {
				t.Errorf("acquisition required an external FIFO opener after its deadline: %v", probeErr)
			}
			if waitErr != nil {
				t.Fatalf("input probe: %v\n%s", waitErr, output.String())
			}
		})
	}
}

func TestArchiveUnixInputProbe(t *testing.T) {
	mode := os.Getenv("LEAFRUN_ARCHIVE_INPUT_PROBE")
	if mode == "" {
		return
	}
	input := os.Getenv("LEAFRUN_ARCHIVE_INPUT_PATH")
	canonical := canonicalArchivePath(t, input)
	parent := os.Getenv("LEAFRUN_ARCHIVE_INPUT_PARENT")
	signals := os.Getenv("LEAFRUN_ARCHIVE_INPUT_SIGNALS")
	if err := os.WriteFile(filepath.Join(signals, "started"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.WriteFile(filepath.Join(signals, "finished"), nil, 0600); err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	defer func() {
		if elapsed := time.Since(started); elapsed > 400*time.Millisecond {
			t.Errorf("native acquisition exceeded deadline: %v", elapsed)
		}
	}()
	if mode == "fifo" {
		tree, err := (ArchiveSource{Path: input, Options: ArchiveOptions{TempDir: parent}}).Open(ctx)
		if tree.Close != nil {
			_ = tree.Close()
		}
		var sourceError *SourceError
		if err == nil || tree.FS != nil || !errors.As(err, &sourceError) || sourceError.Kind != ErrorInput {
			t.Fatalf("FIFO acquisition: %+v %v", tree, err)
		}
		return
	}
	w, err := newOwnedWorkspace(context.Background(), parent)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := w.close(); err != nil {
			t.Error(err)
		}
	}()
	checks, changed := 0, false
	probe := probeContext{Context: ctx, probe: func() {
		checks++
		if checks != 2 {
			return
		}
		changed = true
		if mode == "cancel_after_open" {
			cancel()
			return
		}
		if err := os.Rename(input, input+".retained"); err != nil {
			t.Fatal(err)
		}
		if mode == "opened_fifo_replaced" {
			if err := os.Rename(os.Getenv("LEAFRUN_ARCHIVE_INPUT_REPLACEMENT"), input); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := syscall.Mkfifo(input, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}}
	location, digest, err := captureArchive(probe, w, input)
	if !changed {
		t.Fatalf("fixture did not substitute the path after native open: %v", err)
	}
	if mode == "cancel_after_open" {
		var sourceError *SourceError
		if !errors.Is(err, context.Canceled) || !errors.As(err, &sourceError) || sourceError.Kind != ErrorInput {
			t.Fatalf("acquisition cancellation cause: %v", err)
		}
		return
	}
	if mode == "opened_fifo_replaced" {
		info, statErr := os.Stat(input)
		var sourceError *SourceError
		if statErr != nil || !info.Mode().IsRegular() {
			t.Fatalf("replacement is not regular: %v", statErr)
		}
		if !errors.Is(err, fs.ErrInvalid) || !errors.As(err, &sourceError) || sourceError.Kind != ErrorInput {
			t.Fatalf("opened FIFO accepted after path replacement: %v", err)
		}
		if _, err := w.root.Stat("archive"); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("copied a rejected FIFO: %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(input); err != nil || info.Mode()&fs.ModeNamedPipe == 0 {
		t.Fatalf("replacement is not a FIFO: %v", err)
	}
	capture, err := w.root.Open("archive")
	if err != nil {
		t.Fatal(err)
	}
	actual, readErr := io.ReadAll(capture)
	closeErr := capture.Close()
	expected, originalErr := os.ReadFile(input + ".retained")
	if readErr != nil || closeErr != nil || originalErr != nil || !bytes.Equal(actual, expected) || location != canonical || digest == "" {
		t.Fatalf("opened regular handle changed after path replacement: %v %v %v", readErr, closeErr, originalErr)
	}
}
