//go:build windows

package rtsp

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestWindowsHelperProcess is the short-lived child that the tests below
// start by re-running the test binary; it does nothing.
func TestWindowsHelperProcess(t *testing.T) {}

func startWindowsChild(t *testing.T, name string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

func TestWindowsProcessExistsAfterExit(t *testing.T) {
	cmd := startWindowsChild(t, os.Args[0], "-test.run=^TestWindowsHelperProcess$")
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if processExists(pid) {
		t.Fatalf("processExists(%d) is true for an exited process", pid)
	}
}

func TestWindowsTerminateProcess(t *testing.T) {
	cmd := startWindowsChild(t, "ping", "-n", "30", "127.0.0.1")
	pid := cmd.Process.Pid
	if !processExists(pid) {
		t.Fatalf("processExists(%d) is false for a running process", pid)
	}
	if isServerProcess(pid) {
		t.Fatalf("isServerProcess(%d) is true for a process of another program", pid)
	}
	if err := terminateProcess(pid); err != nil {
		t.Fatal(err)
	}

	// cmd still holds its own handle to the child here.
	deadline := time.Now().Add(5 * time.Second)
	for processExists(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("processExists(%d) still true after terminateProcess", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestWindowsIsServerProcessSelf(t *testing.T) {
	if !isServerProcess(os.Getpid()) {
		t.Fatal("isServerProcess is false for a process of the same executable")
	}
}
