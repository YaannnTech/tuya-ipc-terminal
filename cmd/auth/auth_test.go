package auth

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/mdp/qrterminal"
	"github.com/spf13/cobra"

	"tuya-ipc-terminal/pkg/tuya"
)

func TestPrintQRCode(t *testing.T) {
	var qr bytes.Buffer
	qrterminal.Generate("tuyaSmart--qrLogin?token=test-token", qrterminal.L, &qr)

	for _, tc := range []struct {
		name     string
		printURL bool
		prefix   string
	}{
		{"default", false, ""},
		{"print URL", true, "QR_LOGIN_URL:tuyaSmart--qrLogin?token=test-token\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got bytes.Buffer
			printQRCode(&got, "test-token", tc.printURL)
			if want := tc.prefix + qr.String(); got.String() != want {
				t.Errorf("unexpected QR output: got %q, want %q", got.String(), want)
			}
		})
	}
}

func TestPrintQRCodeFlag(t *testing.T) {
	for _, cmd := range []*cobra.Command{newAddCmd(), newRefreshCmd()} {
		t.Run(cmd.Name(), func(t *testing.T) {
			flag := cmd.Flags().Lookup("print-qr-url")
			if flag == nil || flag.DefValue != "false" {
				t.Fatalf("print-qr-url flag missing or enabled by default")
			}
			if err := cmd.Flags().Set("print-qr-url", "true"); err != nil {
				t.Fatal(err)
			}
			if enabled, err := cmd.Flags().GetBool("print-qr-url"); err != nil || !enabled {
				t.Fatalf("print-qr-url = %t, %v", enabled, err)
			}
		})
	}
}

func TestPasswordLoginErrorHint(t *testing.T) {
	apiErr := &tuya.APIError{Message: "x", Explained: "x"}
	if got := passwordLoginError(apiErr).Error(); !strings.Contains(got, tuya.OEMLoginHint) {
		t.Errorf("API error lacks hint: %q", got)
	}
	if got := passwordLoginError(errors.New("dial tcp: connection refused")).Error(); strings.Contains(got, tuya.OEMLoginHint) {
		t.Errorf("transport error has hint: %q", got)
	}
}
