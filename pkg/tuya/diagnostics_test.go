package tuya

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const (
	testSid    = "SIDSECRET1234567890abcdef"
	testEcode  = "ECODESECRET0987654321"
	testClient = "CLIENTIDSECRET12345"
	testPasswd = "hunter2-very-secret"
	testUID    = "uid-visible-123"
	testCookie = "COOKIESECRET5555"
	testToken  = "LOGINTOKENSECRET777"
)

func TestExplainMessage(t *testing.T) {
	tests := []struct {
		msg      string
		contains string
	}{
		{"账号或密码错误！", "wrong account or password"},
		{"内部错误", "internal error"},
		{"something else", ""},
	}
	for _, tt := range tests {
		got := ExplainMessage(tt.msg)
		if !strings.HasPrefix(got, tt.msg) {
			t.Errorf("ExplainMessage(%q) = %q, original text not kept", tt.msg, got)
		}
		if tt.contains == "" && got != tt.msg {
			t.Errorf("ExplainMessage(%q) = %q, want unchanged", tt.msg, got)
		}
		if !strings.Contains(got, tt.contains) {
			t.Errorf("ExplainMessage(%q) = %q, want %q in it", tt.msg, got, tt.contains)
		}
	}
}

func testLoginServer(t *testing.T, loginMsg string) *httptest.Server {
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

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/login/token":
			fmt.Fprintf(w, `{"success":true,"result":{"token":%q,"pbKey":%q}}`, testToken, pbKey)
		case "/api/private/email/login":
			fmt.Fprintf(w, `{"success":false,"errorMsg":%q}`, loginMsg)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestPasswordLoginExplainsKnownMessages(t *testing.T) {
	for _, msg := range []string{"账号或密码错误！", "内部错误"} {
		server := testLoginServer(t, msg)
		_, err := PasswordLogin(context.Background(), server.Client(), testServerHost(server), "user@example.com", testPasswd, "49")
		if err == nil {
			t.Fatalf("%q: PasswordLogin succeeded, want error", msg)
		}
		if !strings.Contains(err.Error(), msg) {
			t.Errorf("error %q lost the original message %q", err, msg)
		}
		if !strings.Contains(err.Error(), " (") {
			t.Errorf("error %q has no English explanation", err)
		}
		if strings.Contains(err.Error(), testPasswd) {
			t.Errorf("error %q leaks the password", err)
		}
	}
}

func TestGetLoginTokenFailureIsAnError(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"success":false,"errorMsg":"内部错误"}`)
	}))
	defer server.Close()

	resp, err := GetLoginToken(context.Background(), server.Client(), testServerHost(server), "user@example.com", "49")
	if err == nil || resp != nil {
		t.Fatalf("GetLoginToken = %v, %v, want error", resp, err)
	}
	if !strings.Contains(err.Error(), "内部错误") {
		t.Errorf("err = %q, want original message", err)
	}
}

func TestRedactSecret(t *testing.T) {
	if got := RedactSecret(""); got != "(empty)" {
		t.Errorf("empty = %q", got)
	}
	if got := RedactSecret("abcd"); strings.Contains(got, "abcd") {
		t.Errorf("short secret leaked: %q", got)
	}
	got := RedactSecret(testSid)
	if !strings.HasPrefix(got, testSid[:4]) || strings.Contains(got, testSid[:5]) {
		t.Errorf("RedactSecret = %q, want only the first 4 characters", got)
	}
}

func TestCheckLoginSessionDebugOutputHasNoSecrets(t *testing.T) {
	t.Setenv("TUYA_DEBUG", "1")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "session", Value: testCookie})
		switch r.URL.Path {
		case "/api/new/common/homeList":
			fmt.Fprint(w, `{"success":true,"result":[{"gid":7}]}`)
		case "/api/new/common/roomList":
			fmt.Fprint(w, `{"success":true,"result":[{"deviceList":[{"deviceId":"a","category":"sp"},{"deviceId":"b","category":"kg"}]}]}`)
		case "/api/new/playback/shareList":
			fmt.Fprint(w, `{"success":true,"result":{"securityWebCShareInfoList":[{"deviceInfoList":[{"deviceId":"a","category":"sp"}]}]}}`)
		}
	}))
	defer server.Close()

	result := &LoginResult{
		Uid:      testUID,
		Sid:      testSid,
		Ecode:    testEcode,
		ClientId: testClient,
		Domain:   Domain{RegionCode: "EU", MobileApiUrl: "https://a1.tuyaeu.com"},
	}
	var out bytes.Buffer
	CheckLoginSession(context.Background(), &out, server.Client(), testServerHost(server), result, true)

	text := out.String()
	for _, secret := range []string{testSid, testEcode, testClient, testPasswd, testCookie, testToken} {
		if strings.Contains(text, secret) {
			t.Errorf("debug output contains secret %q:\n%s", secret, text)
		}
	}
	for _, want := range []string{testUID, "region code: EU", "sid present: true", "home count: 1", "device count: 2 (cameras: 1)"} {
		if !strings.Contains(text, want) {
			t.Errorf("debug output lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Warning") {
		t.Errorf("unexpected warning with one camera:\n%s", text)
	}
}

func TestCheckLoginSessionWarnsWithoutCameras(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, `{"success":true,"result":[]}`)
	}))
	defer server.Close()

	var out bytes.Buffer
	CheckLoginSession(context.Background(), &out, server.Client(), testServerHost(server), &LoginResult{}, true)
	if !strings.Contains(out.String(), "no cameras") || !strings.Contains(out.String(), OEMLoginHint) {
		t.Errorf("want warning with OEM hint, got:\n%s", out.String())
	}
	if strings.Contains(out.String(), "TUYA_DEBUG login result") {
		t.Errorf("debug output without TUYA_DEBUG:\n%s", out.String())
	}

	requests.Store(0)
	out.Reset()
	CheckLoginSession(context.Background(), &out, server.Client(), testServerHost(server), &LoginResult{}, false)
	if out.Len() != 0 || requests.Load() != 0 {
		t.Errorf("QR login without debug must be silent and make no requests, got %q, %d requests", out.String(), requests.Load())
	}
}

func TestIsCameraCategory(t *testing.T) {
	tests := []struct {
		category string
		want     bool
	}{
		{category: "sp", want: true},
		{category: "dghsxj", want: true},
		{category: "cdsxj", want: true},
		{category: "sp_wnq", want: true},
		{category: "ksdjml", want: true},
		{category: "unknown", want: false},
		{category: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.category, func(t *testing.T) {
			if got := IsCameraCategory(tt.category); got != tt.want {
				t.Errorf("IsCameraCategory(%q) = %t, want %t", tt.category, got, tt.want)
			}
		})
	}
}
