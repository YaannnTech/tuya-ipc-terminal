//go:build windows

package rtsp

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

const (
	createNewProcessGroup = 0x00000200
	detachedProcess       = 0x00000008
)

func detachedProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: createNewProcessGroup | detachedProcess}
}

// processExists reports whether the process is still running. OpenProcess
// also succeeds for an exited process while any handle to it is open, so
// the process is waited on with a zero timeout instead.
func processExists(pid int) bool {
	handle, err := windows.OpenProcess(
		windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)
	event, err := windows.WaitForSingleObject(handle, 0)
	return err == nil && event == uint32(windows.WAIT_TIMEOUT)
}

// isServerProcess reports whether pid is a running process of this
// executable, so a PID that was reused by another program after a crash is
// not mistaken for the server. Only the base name is compared, so another
// process of this executable (not "rtsp start") still passes. If the image
// name cannot be read (e.g. access denied), it trusts the record, like the
// Unix version without /proc.
func isServerProcess(pid int) bool {
	if !processExists(pid) {
		return false
	}
	self, err := os.Executable()
	if err != nil {
		return true
	}
	handle, err := windows.OpenProcess(
		windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return true
	}
	defer windows.CloseHandle(handle)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(buf))
	err = windows.QueryFullProcessImageName(handle, 0, &buf[0], &size)
	if err != nil {
		return true
	}
	image := windows.UTF16ToString(buf[:size])
	return strings.EqualFold(filepath.Base(image), filepath.Base(self))
}

// terminateProcess ends the server. Windows has no SIGTERM for console
// processes in another group, so this is a hard stop.
func terminateProcess(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	defer process.Release()
	return process.Kill()
}
