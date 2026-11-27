package rtsp

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func processIdentity(pid int) (string, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", err
	}
	if info.Proc.P_pid != int32(pid) || info.Proc.P_starttime.Sec == 0 {
		return "", fmt.Errorf("missing process start time")
	}
	start := info.Proc.P_starttime
	return fmt.Sprintf("%d.%06d", start.Sec, start.Usec), nil
}
