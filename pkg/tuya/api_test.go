package tuya

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testServerHost(server *httptest.Server) string {
	return strings.TrimPrefix(server.URL, "https://")
}

func TestAPIRequestStopsWhenContextIsCancelled(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)

	start := time.Now()
	_, err := GetAppInfo(ctx, server.Client(), testServerHost(server))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("request returned %v after cancel, want right away", elapsed)
	}
}

func TestPollForLoginStopsWhenContextIsCancelled(t *testing.T) {
	var polls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		polls.Add(1)
		w.Write([]byte(`{"success":false,"result":null}`))
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(1200*time.Millisecond, cancel)

	start := time.Now()
	_, err := PollForLogin(ctx, server.Client(), testServerHost(server), "token")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 1600*time.Millisecond {
		t.Fatalf("polling returned %v after start, want right after the cancel at 1.2s, not after the 1s wait", elapsed)
	}
	if n := polls.Load(); n < 2 {
		t.Fatalf("polls = %d, want polling to go on while the QR code is not scanned", n)
	}
}
