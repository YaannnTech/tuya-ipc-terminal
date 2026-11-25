package auth

import (
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mdp/qrterminal"
	"github.com/spf13/cobra"

	"tuya-ipc-terminal/pkg/storage"
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

const (
	testRegion   = "test-region"
	testEmail    = "user@example.com"
	testPassword = "correct horse battery staple"
)

// loginEnv fakes the Tuya password login: the server accepts testPassword
// only. It counts the login requests and sets up an empty storage directory.
type loginEnv struct {
	logins int
	sm     *storage.StorageManager
}

func newLoginEnv(t *testing.T) *loginEnv {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pbKey := base64.StdEncoding.EncodeToString(der)
	want := md5.Sum([]byte(testPassword))

	env := &loginEnv{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/login/token":
			fmt.Fprintf(w, `{"success":true,"result":{"token":"t","pbKey":%q}}`, pbKey)
		case "/api/private/email/login":
			env.logins++
			var req tuya.PasswordLoginRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
			}
			enc, _ := hex.DecodeString(req.Passwd)
			plain, err := rsa.DecryptPKCS1v15(nil, key, enc)
			if err != nil || string(plain) != hex.EncodeToString(want[:]) {
				fmt.Fprint(w, `{"success":false,"errorMsg":"账号或密码错误！"}`)
				return
			}
			fmt.Fprintf(w, `{"success":true,"result":{"email":%q,"nickname":"nick","uid":"uid1"}}`, testEmail)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	oldRegions, oldClient, oldStdin := AvailableRegions, newHTTPClient, stdin
	AvailableRegions = append([]tuya.Region{{
		Name:      testRegion,
		Host:      strings.TrimPrefix(server.URL, "https://"),
		Continent: "49",
	}}, AvailableRegions...)
	newHTTPClient = func() *http.Client {
		client := createHTTPClientWithSession(nil)
		client.Transport = server.Client().Transport
		return client
	}
	t.Cleanup(func() { AvailableRegions, newHTTPClient, stdin = oldRegions, oldClient, oldStdin })

	t.Chdir(t.TempDir())
	env.sm, err = storage.NewStorageManager()
	if err != nil {
		t.Fatal(err)
	}
	SetStorageManager(env.sm)
	return env
}

func (e *loginEnv) addExisting(t *testing.T) {
	t.Helper()
	if err := e.sm.SaveUser(testRegion, testEmail, &tuya.SessionData{}); err != nil {
		t.Fatal(err)
	}
}

// run executes "auth <args>" with stdin as the standard input and returns
// the process exit code.
func (e *loginEnv) run(t *testing.T, input string, args ...string) (int, error) {
	t.Helper()
	stdin = strings.NewReader(input)
	cmd := NewAuthCmd()
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(args)
	err := cmd.Execute()
	return ExitCode(err), err
}

func TestPasswordStdinAddAndRefresh(t *testing.T) {
	env := newLoginEnv(t)

	code, err := env.run(t, testPassword+"\n", "add", "--password", "--password-stdin", testRegion, testEmail)
	if code != 0 {
		t.Fatalf("add: exit %d, %v", code, err)
	}
	if user, _ := env.sm.GetUser(testRegion, testEmail); user == nil {
		t.Fatal("add did not store the user")
	}

	code, err = env.run(t, testPassword+"\r\nignored\n", "refresh", "--password", "--password-stdin", testRegion, testEmail)
	if code != 0 {
		t.Fatalf("refresh: exit %d, %v", code, err)
	}
}

func TestPasswordStdinKeepsPasswordVerbatim(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"LF", "a b \nrest\n", "a b "},
		{"CRLF", "pw\r\n", "pw"},
		{"no newline", "pw", "pw"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readPasswordLine(strings.NewReader(tc.input))
			if err != nil || got != tc.want {
				t.Errorf("readPasswordLine(%q) = %q, %v, want %q", tc.input, got, err, tc.want)
			}
		})
	}
	for _, input := range []string{"", "\n"} {
		if _, err := readPasswordLine(strings.NewReader(input)); err == nil {
			t.Errorf("readPasswordLine(%q) succeeded, want error", input)
		}
	}
}

func TestExitCodes(t *testing.T) {
	const (
		target = " " + testRegion + " " + testEmail
		right  = testPassword + "\n"
		add    = "add --password --password-stdin"
		upd    = "refresh --password --password-stdin"
	)
	for _, tc := range []struct {
		name     string
		existing bool
		input    string
		args     string
		want     int
		logins   int
	}{
		{"success", false, right, add + target, 0, 1},
		{"wrong password", false, "nope\n", add + target, ExitBadCredentials, 1},
		{"refresh wrong password", true, "nope\n", upd + target, ExitBadCredentials, 1},
		{"exists without --yes", true, right, add + target, ExitNotConfirmed, 0},
		{"exists with --yes", true, right, add + " --yes" + target, 0, 1},
		{"exists with -y", true, right, add + " -y" + target, 0, 1},
		{"refresh accepts --yes", true, right, upd + " --yes" + target, 0, 1},
		{"stdin without password", false, right, "add --password-stdin" + target, ExitUsage, 0},
		{"refresh stdin without password", true, right, "refresh --password-stdin" + target, ExitUsage, 0},
		{"empty stdin", false, "", add + target, ExitUsage, 0},
		{"missing argument", false, "", "add --password " + testRegion, ExitUsage, 0},
		{"unknown flag", false, "", "add --nope" + target, ExitUsage, 0},
		{"invalid region", false, "", "add --password nowhere " + testEmail, ExitUsage, 0},
		{"invalid email", false, "", "add --password " + testRegion + " nope", ExitUsage, 0},
		{"unknown country code", false, "", "add --password --country-code 0" + target, ExitUsage, 0},
		{"refresh of unknown user", false, right, upd + target, ExitError, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newLoginEnv(t)
			if tc.existing {
				env.addExisting(t)
			}
			code, err := env.run(t, tc.input, strings.Fields(tc.args)...)
			if code != tc.want {
				t.Errorf("exit code = %d (%v), want %d", code, err, tc.want)
			}
			if env.logins != tc.logins {
				t.Errorf("login requests = %d, want %d", env.logins, tc.logins)
			}
			if err != nil && strings.Contains(err.Error(), testPassword) {
				t.Errorf("error leaks the password: %v", err)
			}
		})
	}
}

func TestExitCodeOfPlainError(t *testing.T) {
	if got := ExitCode(errors.New("x")); got != ExitError {
		t.Errorf("ExitCode(plain) = %d, want %d", got, ExitError)
	}
	if got := ExitCode(fmt.Errorf("wrapped: %w", usageError(errors.New("x")))); got != ExitUsage {
		t.Errorf("ExitCode(wrapped usage) = %d, want %d", got, ExitUsage)
	}
	if got := ExitCode(nil); got != 0 {
		t.Errorf("ExitCode(nil) = %d, want 0", got)
	}
}
