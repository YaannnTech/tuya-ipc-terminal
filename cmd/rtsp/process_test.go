//go:build !windows

package rtsp

import (
	"bufio"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"
)

// startFakeServer starts a process whose command line contains "rtsp start",
// like a real server, and that exits cleanly on SIGTERM.
func startFakeServer(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sh", "-c", `trap "exit 0" TERM; while :; do sleep 0.05; done`, "rtsp", "start")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

func TestRunningServersRemovesRecordsOfExitedProcesses(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if err := writeServerRecord(dir, serverRecord{PID: cmd.Process.Pid, Port: 8554}); err != nil {
		t.Fatal(err)
	}

	servers, err := runningServers(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 0 {
		t.Fatalf("runningServers() = %v, want none for an exited process", servers)
	}
	if _, err := os.Stat(recordPath(dir, 8554)); !os.IsNotExist(err) {
		t.Fatal("stale record was not removed")
	}
}

func TestRunningServersIgnoresReusedPID(t *testing.T) {
	if _, err := os.Stat("/proc/self/cmdline"); err != nil {
		t.Skip("needs /proc")
	}
	dir := t.TempDir()
	// The test binary is alive but is not "rtsp start", like a PID that was
	// reused by another program after the server crashed.
	if err := writeServerRecord(dir, serverRecord{PID: os.Getpid(), Port: 8554}); err != nil {
		t.Fatal(err)
	}

	servers, err := runningServers(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 0 {
		t.Fatalf("runningServers() = %v, want none for a PID of another program", servers)
	}
}

func TestStopServerEndsTheProcessAndRemovesItsRecord(t *testing.T) {
	dir := t.TempDir()
	server := startFakeServer(t)
	rec := serverRecord{PID: server.Process.Pid, Port: 8555, Started: time.Now()}
	if err := writeServerRecord(dir, rec); err != nil {
		t.Fatal(err)
	}

	found, err := findServer(dir, 8555)
	if err != nil || found == nil || found.PID != rec.PID {
		t.Fatalf("findServer() = %v, %v; want the running server", found, err)
	}

	exited := make(chan struct{})
	go func() {
		_ = server.Wait()
		close(exited)
	}()

	if err := stopServer(dir, rec); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("server process still running after stopServer")
	}
	if _, err := os.Stat(recordPath(dir, 8555)); !os.IsNotExist(err) {
		t.Fatal("record still there after the server stopped")
	}
}

func TestRemoveServerRecordKeepsTheRecordOfANewerServer(t *testing.T) {
	dir := t.TempDir()
	if err := writeServerRecord(dir, serverRecord{PID: 222, Port: 8554}); err != nil {
		t.Fatal(err)
	}

	removeServerRecord(dir, 8554, 111) // an older server exiting late

	if _, err := os.Stat(recordPath(dir, 8554)); err != nil {
		t.Fatalf("record of the newer server was removed: %v", err)
	}
}

func listenOnce(t *testing.T, answer string) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = bufio.NewReader(conn).ReadString('\n')
		_, _ = conn.Write([]byte(answer))
	}()
	return listener.Addr().(*net.TCPAddr).Port
}

func TestProbeRTSP(t *testing.T) {
	// Any RTSP answer counts, even 404 for the root path.
	if err := probeRTSP("127.0.0.1", listenOnce(t, "RTSP/1.0 404 Not Found\r\nCSeq: 1\r\n\r\n"), time.Second); err != nil {
		t.Fatalf("probeRTSP() with an RTSP server = %v, want nil", err)
	}
	if err := probeRTSP("127.0.0.1", listenOnce(t, "HTTP/1.1 400 Bad Request\r\n\r\n"), time.Second); err == nil {
		t.Fatal("probeRTSP() accepted a non-RTSP server")
	}
	// A server on all interfaces is probed via loopback.
	for _, address := range []string{"", "0.0.0.0"} {
		if err := probeRTSP(address, listenOnce(t, "RTSP/1.0 200 OK\r\nCSeq: 1\r\n\r\n"), time.Second); err != nil {
			t.Fatalf("probeRTSP(%q) = %v, want nil", address, err)
		}
	}
}

func TestRecordWithOwnPIDIsStale(t *testing.T) {
	// In a container the server's own command line is "rtsp start", so the
	// process check alone would accept the record.
	serverProcessCheck = func(int) bool { return true }
	defer func() { serverProcessCheck = isServerProcess }()

	dir := t.TempDir()
	// After a container restart the new server can get the PID of the old one
	// that left its record behind; it must not see itself as already running.
	if err := writeServerRecord(dir, serverRecord{PID: os.Getpid(), Port: 8554}); err != nil {
		t.Fatal(err)
	}
	rec, err := findServer(dir, 8554)
	if err != nil {
		t.Fatal(err)
	}
	if rec != nil {
		t.Fatalf("findServer() = %+v, want nil: a record with our own PID is stale", rec)
	}
}
