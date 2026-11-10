package rtsp

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// serverRecord describes a running "rtsp start" process. It is written to
// .tuya-data/rtsp-<port>.pid so that "rtsp stop" and "rtsp status", which run
// in their own processes, can find the server.
type serverRecord struct {
	PID     int       `json:"pid"`
	Port    int       `json:"port"`
	Address string    `json:"address,omitempty"`
	Started time.Time `json:"started"`
	LogFile string    `json:"logFile,omitempty"`
}

// serverProcessCheck is isServerProcess; tests replace it.
var serverProcessCheck = isServerProcess

const (
	stopTimeout   = 15 * time.Second
	daemonTimeout = 10 * time.Second
)

func recordPath(dataDir string, port int) string {
	return filepath.Join(dataDir, fmt.Sprintf("rtsp-%d.pid", port))
}

func logPath(dataDir string, port int) string {
	return filepath.Join(dataDir, fmt.Sprintf("rtsp-%d.log", port))
}

func writeServerRecord(dataDir string, rec serverRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return os.WriteFile(recordPath(dataDir, rec.Port), data, 0600)
}

// removeServerRecord removes the record of this process only, so a server
// that exits late cannot delete the record of a newer one on the same port.
func removeServerRecord(dataDir string, port, pid int) {
	path := recordPath(dataDir, port)
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var rec serverRecord
	if json.Unmarshal(data, &rec) == nil && rec.PID == pid {
		_ = os.Remove(path)
	}
}

// runningServers returns the records of servers that are still running and
// removes records whose process is gone.
func runningServers(dataDir string) ([]serverRecord, error) {
	paths, err := filepath.Glob(filepath.Join(dataDir, "rtsp-*.pid"))
	if err != nil {
		return nil, err
	}

	var running []serverRecord
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var rec serverRecord
		// A record with our own PID is stale too: in a container, a restarted
		// server can get the same PID as the one that left the record behind.
		if err := json.Unmarshal(data, &rec); err != nil || rec.PID <= 0 || rec.PID == os.Getpid() ||
			!serverProcessCheck(rec.PID) {
			_ = os.Remove(path) // stale or broken
			continue
		}
		running = append(running, rec)
	}

	sort.Slice(running, func(i, j int) bool { return running[i].Port < running[j].Port })
	return running, nil
}

func findServer(dataDir string, port int) (*serverRecord, error) {
	servers, err := runningServers(dataDir)
	if err != nil {
		return nil, err
	}
	for _, rec := range servers {
		if rec.Port == port {
			return &rec, nil
		}
	}
	return nil, nil
}

// probeRTSP checks that something answers RTSP on the server's listen
// address. For all interfaces (empty, 0.0.0.0 or ::) it uses the loopback
// address.
func probeRTSP(address string, port int, timeout time.Duration) error {
	host := address
	if host == "" {
		host = "127.0.0.1"
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		if ip.To4() == nil {
			host = "::1"
		} else {
			host = "127.0.0.1"
		}
	}
	endpoint := net.JoinHostPort(host, fmt.Sprint(port))
	conn, err := net.DialTimeout("tcp", endpoint, timeout)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))

	request := fmt.Sprintf("OPTIONS rtsp://%s/ RTSP/1.0\r\n"+
		"CSeq: 1\r\nUser-Agent: tuya-ipc-terminal-status\r\n\r\n", endpoint)
	if _, err := conn.Write([]byte(request)); err != nil {
		return err
	}

	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return err
	}
	if !strings.HasPrefix(line, "RTSP/1.0 ") {
		return fmt.Errorf("unexpected answer %q", strings.TrimSpace(line))
	}
	return nil
}

// stopServer asks the server to shut down and waits for it to exit.
func stopServer(dataDir string, rec serverRecord) error {
	if err := terminateProcess(rec.PID); err != nil {
		return fmt.Errorf("failed to signal process %d: %v", rec.PID, err)
	}

	deadline := time.Now().Add(stopTimeout)
	for time.Now().Before(deadline) {
		if !processExists(rec.PID) {
			removeServerRecord(dataDir, rec.Port, rec.PID)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("process %d did not stop within %s", rec.PID, stopTimeout)
}

// startDaemon runs "rtsp start" again as a detached background process and
// waits until it serves RTSP.
func startDaemon(dataDir string, port int, address string) (*serverRecord, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}

	logFile := logPath(dataDir, port)
	out, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	defer out.Close()

	args := []string{"rtsp", "start", "--port", fmt.Sprint(port)}
	if address != "" {
		args = append(args, "--address", address)
	}
	cmd := exec.Command(exe, args...)
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.SysProcAttr = detachedProcAttr()
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	deadline := time.After(daemonTimeout)
	for {
		select {
		case err := <-exited:
			return nil, fmt.Errorf("background server exited (%v), see %s", err, logFile)
		case <-deadline:
			return nil, fmt.Errorf("background server did not start within %s, see %s", daemonTimeout, logFile)
		case <-time.After(200 * time.Millisecond):
		}

		rec, err := findServer(dataDir, port)
		if err == nil && rec != nil && rec.PID == cmd.Process.Pid && probeRTSP(address, port, time.Second) == nil {
			rec.LogFile = logFile
			_ = writeServerRecord(dataDir, *rec)
			_ = cmd.Process.Release()
			return rec, nil
		}
	}
}

var errAlreadyRunning = errors.New("already running")
