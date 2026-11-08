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

// isServerProcess reports whether pid is a running "rtsp start" process. Where
// /proc is available it checks the command line, so a PID that was reused by
// another program after a crash is not mistaken for the server.
func isServerProcess(pid int) bool {
	if !processExists(pid) {
		return false
	}
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return true // no /proc (e.g. macOS): trust the record
	}
	args := strings.Split(string(cmdline), "\x00")
	hasRTSP, hasStart := false, false
	for _, arg := range args {
		hasRTSP = hasRTSP || arg == "rtsp"
		hasStart = hasStart || arg == "start"
	}
	return hasRTSP && hasStart
}

func terminateProcess(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}
