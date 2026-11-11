package auth

import (
	"errors"
	"strings"
	"testing"

	"tuya-ipc-terminal/pkg/tuya"
)

func TestPasswordLoginErrorHint(t *testing.T) {
	apiErr := &tuya.APIError{Message: "x", Explained: "x"}
	if got := passwordLoginError(apiErr).Error(); !strings.Contains(got, tuya.OEMLoginHint) {
		t.Errorf("API error lacks hint: %q", got)
	}
	if got := passwordLoginError(errors.New("dial tcp: connection refused")).Error(); strings.Contains(got, tuya.OEMLoginHint) {
		t.Errorf("transport error has hint: %q", got)
	}
}
