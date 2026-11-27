//go:build !windows

package rtsp

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
)

func detachedProcAttr() *syscall.SysProcAttr {
	// New session: the server keeps running after the terminal closes.
	return &syscall.SysProcAttr{Setsid: true}
}

func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// isServerProcess reports whether the record identifies a running server.
// When /proc is readable, the command line must contain "rtsp" and "start".
func isServerProcess(rec serverRecord) bool {
	if !processExists(rec.PID) {
		return false
	}
	if rec.ProcStart != "" {
		identity, err := processIdentity(rec.PID)
		if !matchesProcessIdentity(rec.ProcStart, identity, err) {
			return false
		}
	}
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", rec.PID))
	if err != nil {
		// No /proc (e.g. macOS), or cmdline unavailable: Trust the record.
		return true
	}
	args := strings.Split(string(cmdline), "\x00")
	hasRTSP, hasStart := false, false
	for _, arg := range args {
		hasRTSP = hasRTSP || arg == "rtsp"
		hasStart = hasStart || arg == "start"
	}
	return hasRTSP && hasStart
}

// terminateProcess checks the record again and then signals the PID. If the
// server exits in the short time between the check and the signal and its
// PID is reused at once, the signal can still reach the new process.
func terminateProcess(rec serverRecord) error {
	if !isServerProcess(rec) {
		return fmt.Errorf("process %d no longer matches the server record", rec.PID)
	}
	return syscall.Kill(rec.PID, syscall.SIGTERM)
}
