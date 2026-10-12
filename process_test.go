package integrations

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestNativeProcessHelper(t *testing.T) {
	mode := os.Getenv("LEAFRUN_PROCESS_HELPER")
	if mode == "" {
		return
	}
	ready := os.Getenv("LEAFRUN_PROCESS_READY")
	if mode == "child" || mode == "grandchild" {
		file, err := os.OpenFile(ready+".held", os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		descendants := ""
		if mode == "child" {
			child := exec.Command(os.Args[0], "-test.run=^TestNativeProcessHelper$")
			hideFixtureProcess(child)
			child.Env = append(os.Environ(), "LEAFRUN_PROCESS_HELPER=grandchild", "LEAFRUN_PROCESS_READY="+ready+".grand")
			child.Stdout = os.Stdout
			child.Stderr = os.Stderr
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(10 * time.Second)
			for {
				data, err := os.ReadFile(ready + ".grand")
				if err == nil && len(strings.Fields(string(data))) == 1 {
					descendants = string(data)
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("grandchild not ready")
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
		if err := os.WriteFile(ready, []byte(fmt.Sprintf("%d %s", os.Getpid(), descendants)), 0600); err != nil {
			t.Fatal(err)
		}
		for {
			time.Sleep(time.Second)
		}
	}
	child := exec.Command(os.Args[0], "-test.run=^TestNativeProcessHelper$")
	hideFixtureProcess(child)
	child.Env = append(os.Environ(), "LEAFRUN_PROCESS_HELPER=child")
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if data, err := os.ReadFile(ready); err == nil && len(strings.Fields(string(data))) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child not ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if ack := os.Getenv("LEAFRUN_PROCESS_ACK"); ack != "" {
		for {
			if _, err := os.Stat(ack); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("observer did not retain descendant identities")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if mode == "parent-normal" {
		return
	}
	if mode == "parent-spawn" {
		for i := range 8 {
			child := exec.Command(os.Args[0], "-test.run=^TestNativeProcessHelper$")
			hideFixtureProcess(child)
			child.Env = append(os.Environ(), "LEAFRUN_PROCESS_HELPER=grandchild", fmt.Sprintf("LEAFRUN_PROCESS_READY=%s.late%d", ready, i))
			child.Stdout = os.Stdout
			child.Stderr = os.Stderr
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
		}
	}
	for {
		time.Sleep(time.Second)
	}
}
func TestNativeProcessTreeCleanup(t *testing.T) {
	for _, mode := range []string{"parent-normal", "parent-cancel", "parent-reader-failure", "parent-spawn"} {
		t.Run(mode, func(t *testing.T) {
			temp := t.TempDir()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			owned, err := newOwnedWorkspace(ctx, temp)
			if err != nil {
				t.Fatal(err)
			}
			defer owned.close()
			ready := filepath.Join(owned.root.Name(), "ready")
			ack := ready + ".ack"
			readySignal := make(chan struct{})
			var observed []fixtureProcess
			var observationErr error
			go func() {
				defer close(readySignal)
				for {
					if data, err := os.ReadFile(ready); err == nil && len(strings.Fields(string(data))) == 2 {
						var pids []int
						for _, field := range strings.Fields(string(data)) {
							pid, err := strconv.Atoi(field)
							if err != nil {
								observationErr = err
								cancel()
								return
							}
							pids = append(pids, pid)
						}
						observed, observationErr = captureFixtureProcesses(pids)
						if observationErr != nil {
							cancel()
							return
						}
						if observationErr = os.WriteFile(ack, []byte("identities retained"), 0600); observationErr != nil {
							cancel()
							return
						}
						if mode == "parent-spawn" {
							for {
								if data, err := os.ReadFile(ready + ".late0"); err == nil && len(strings.Fields(string(data))) == 1 {
									pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
									if err != nil {
										observationErr = err
										cancel()
										return
									}
									late, err := captureFixtureProcesses([]int{pid})
									if err != nil {
										observationErr = err
										cancel()
										return
									}
									observed = append(observed, late...)
									break
								}
								select {
								case <-ctx.Done():
									observationErr = ctx.Err()
									return
								case <-time.After(10 * time.Millisecond):
								}
							}
						}
						if mode == "parent-cancel" || mode == "parent-spawn" {
							cancel()
						}
						return
					}
					select {
					case <-ctx.Done():
						return
					case <-time.After(10 * time.Millisecond):
					}
				}
			}()
			cmd := exec.Command(os.Args[0], "-test.run=^TestNativeProcessHelper$")
			cmd.Env = append(os.Environ(), "LEAFRUN_PROCESS_HELPER="+mode, "LEAFRUN_PROCESS_READY="+ready, "LEAFRUN_PROCESS_ACK="+ack)
			sentinel := errors.New("synthetic reader failure")
			start := time.Now()
			err = runNative(ctx, cmd, func(reader io.Reader) error {
				if mode == "parent-reader-failure" {
					<-readySignal
					return sentinel
				}
				_, err := io.Copy(io.Discard, reader)
				return err
			})
			<-readySignal
			defer func() {
				for _, process := range observed {
					_ = process.close()
				}
			}()
			if observationErr != nil {
				t.Fatal(observationErr)
			}
			if mode == "parent-normal" && err != nil {
				t.Fatal(err)
			}
			if (mode == "parent-cancel" || mode == "parent-spawn") && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel cause: %v", err)
			}
			if mode == "parent-reader-failure" && !errors.Is(err, sentinel) {
				t.Fatalf("reader cause: %v", err)
			}
			if time.Since(start) > 10*time.Second {
				t.Fatal("descendant kept pipes open")
			}
			wanted := 2
			if mode == "parent-spawn" {
				wanted = 3
			}
			if len(observed) != wanted {
				t.Fatalf("original descendant identities: %d", len(observed))
			}
			for _, process := range observed {
				running, err := process.running()
				if err != nil {
					t.Fatal(err)
				}
				if running {
					t.Fatalf("original descendant %d survived runNative", process.pid)
				}
			}
			if err := owned.close(); err != nil {
				t.Fatal(err)
			}
			items, err := os.ReadDir(temp)
			if err != nil || len(items) != 0 {
				t.Fatalf("full owned removal: %v %v", items, err)
			}
		})
	}
}

func TestNativeRunMissingExecutablePreservesCauseAndCleanup(t *testing.T) {
	temp := t.TempDir()
	w, err := newOwnedWorkspace(context.Background(), temp)
	if err != nil {
		t.Fatal(err)
	}
	defer w.close()
	err = runNative(context.Background(), exec.Command(filepath.Join(temp, "missing-native-command")), func(r io.Reader) error { _, err := io.Copy(io.Discard, r); return err })
	if !errors.Is(err, exec.ErrNotFound) && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("native start cause: %v", err)
	}
	if err := w.close(); err != nil {
		t.Fatal(err)
	}
	items, err := os.ReadDir(temp)
	if err != nil || len(items) != 0 {
		t.Fatalf("start-failure cleanup: %v %v", items, err)
	}
}
