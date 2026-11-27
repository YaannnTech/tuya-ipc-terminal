//go:build windows

package rtsp

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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

// isServerProcess reports whether the record identifies a running server.
// When the image name can be read, it checks the executable's base name, so
// another process of this executable can still pass the name check.
// If the handle or image name cannot be read, the record is accepted; when a
// handle is available, the identity is checked first.
func isServerProcess(rec serverRecord) bool {
	if !processExists(rec.PID) {
		return false
	}
	handle, err := windows.OpenProcess(
		windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE,
		false, uint32(rec.PID))
	if err != nil {
		return true
	}
	defer windows.CloseHandle(handle)
	return matchesServerHandle(handle, rec)
}

func matchesServerHandle(handle windows.Handle, rec serverRecord) bool {
	event, err := windows.WaitForSingleObject(handle, 0)
	if err != nil || event != uint32(windows.WAIT_TIMEOUT) {
		return false
	}
	if rec.ProcStart != "" {
		identity, err := processHandleIdentity(handle)
		if !matchesProcessIdentity(rec.ProcStart, identity, err) {
			return false
		}
	}
	self, err := os.Executable()
	if err != nil {
		return true
	}
	buf := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(buf))
	err = windows.QueryFullProcessImageName(handle, 0, &buf[0], &size)
	if err != nil {
		return true
	}
	image := windows.UTF16ToString(buf[:size])
	return strings.EqualFold(filepath.Base(image), filepath.Base(self))
}

func processIdentity(pid int) (string, error) {
	handle, err := windows.OpenProcess(
		windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(handle)
	return processHandleIdentity(handle)
}

func processHandleIdentity(handle windows.Handle) (string, error) {
	var created, exited, kernel, user windows.Filetime
	err := windows.GetProcessTimes(handle, &created, &exited, &kernel, &user)
	if err != nil {
		return "", err
	}
	value := uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime)
	return strconv.FormatUint(value, 10), nil
}

// terminateProcess ends the server. Windows has no SIGTERM for console
// processes in another group, so this is a hard stop.
func terminateProcess(rec serverRecord) error {
	handle, err := windows.OpenProcess(
		windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE|
			windows.PROCESS_TERMINATE, false, uint32(rec.PID))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	if !matchesServerHandle(handle, rec) {
		return fmt.Errorf("process %d no longer matches the server record", rec.PID)
	}
	return windows.TerminateProcess(handle, 1)
}
