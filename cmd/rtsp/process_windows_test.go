//go:build windows

package rtsp

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestWindowsHelperProcess is a child started by re-running the test binary.
func TestWindowsHelperProcess(t *testing.T) {
	if os.Getenv("RTSP_TEST_CHILD") == "1" {
		time.Sleep(30 * time.Second)
	}
}

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
	rec := serverRecord{PID: pid, Started: time.Now()}
	if isServerProcess(rec) {
		t.Fatalf("isServerProcess(%d) is true for a process of another program", pid)
	}
	if err := terminateProcess(rec); err == nil {
		t.Fatal("terminateProcess accepted a process of another program")
	}
	if err := cmd.Process.Kill(); err != nil {
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
	if !isServerProcess(serverRecord{PID: os.Getpid(), Started: time.Now()}) {
		t.Fatal("isServerProcess is false for a process of the same executable")
	}
}

func TestWindowsReusedPIDAndTermination(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestWindowsHelperProcess$")
	cmd.Env = append(os.Environ(), "RTSP_TEST_CHILD=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	self, err := processIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	again, err := processIdentity(os.Getpid())
	if err != nil || self == "" || again != self {
		t.Fatalf("process identity changed: %q, %q, %v", self, again, err)
	}
	child, err := processIdentity(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if child == self {
		t.Fatal("child has the same process identity")
	}
	selfRec := serverRecord{PID: os.Getpid(), ProcStart: child}
	if isServerProcess(selfRec) {
		t.Fatal("accepted a different identity for our PID")
	}
	selfRec.ProcStart = self
	if !isServerProcess(selfRec) {
		t.Fatal("rejected the matching identity for our PID")
	}
	selfRec.ProcStart = ""
	if !isServerProcess(selfRec) {
		t.Fatal("rejected an older record without an identity")
	}
	rec := serverRecord{
		PID: cmd.Process.Pid, Port: 8555, ProcStart: self,
	}
	if isServerProcess(rec) {
		t.Fatal("accepted a process with a different identity")
	}
	if err := terminateProcess(rec); err == nil {
		t.Fatal("terminated a process with a different identity")
	}
	if !processExists(rec.PID) {
		t.Fatal("the unrelated process was terminated")
	}
	rec.ProcStart = ""
	if !isServerProcess(rec) {
		t.Fatal("record without an identity was rejected")
	}
	rec.ProcStart = child
	if !isServerProcess(rec) {
		t.Fatal("valid record was rejected")
	}
	if err := terminateProcess(rec); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("terminated child exited successfully")
	}
}
