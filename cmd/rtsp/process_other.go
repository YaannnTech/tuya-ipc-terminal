//go:build !windows && !linux && !darwin

package rtsp

import "errors"

func processIdentity(int) (string, error) {
	return "", errors.New("process identity unavailable")
}
