package rtsp

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"tuya-ipc-terminal/pkg/storage"
)

func renewalBridge(t *testing.T) (*WebRTCBridge, *bridgeSession) {
	t.Helper()
	bridge := newTestBridge()
	client, server := net.Pipe()
	t.Cleanup(func() {
		bridge.Stop()
		client.Close()
		server.Close()
	})
	if err := bridge.rtpForwarder.AddTCPClient("viewer", server, 0, 2, 4); err != nil {
		t.Fatal(err)
	}
	s := bridge.session.Load()
	s.isHEVC = true
	if err := s.markReady(); err != nil {
		t.Fatal(err)
	}
	bridge.connected = true
	bridge.renewalDelay = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	return bridge, s
}

func awaitSessionReady(t *testing.T, bridge *WebRTCBridge, previous *bridgeSession) *bridgeSession {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s := bridge.session.Load()
		if s != previous {
			s.stateMutex.Lock()
			ready := s.ready
			s.stateMutex.Unlock()
			if ready {
				return s
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("renewal did not finish")
	return nil
}

func TestRenewalKeepsClientsAndIgnoresStaleCallbacks(t *testing.T) {
	bridge, old := renewalBridge(t)
	forwarder := bridge.rtpForwarder
	var attempts atomic.Int32
	bridge.setupSession = func(s *bridgeSession) error {
		attempts.Add(1)
		s.isHEVC = true
		return nil
	}
	old.handleError("data_channel_close", errors.New("closed"))
	current := awaitSessionReady(t, bridge, old)
	if bridge.rtpForwarder != forwarder || forwarder.GetClientCount() != 1 {
		t.Fatal("renewal replaced the forwarder or removed a client")
	}
	old.stateMutex.Lock()
	old.failure = nil
	old.stateMutex.Unlock()
	old.handleError("peer_failed", errors.New("late callback"))
	old.stateMutex.Lock()
	staleFailure := old.failure
	old.stateMutex.Unlock()
	if staleFailure != nil {
		t.Fatal("stale callback changed the old session")
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("stale callback started %d attempts", got)
	}

	current.handleError("data_channel_error", errors.New("closed again"))
	awaitSessionReady(t, bridge, current)
	if got := attempts.Load(); got != 2 {
		t.Fatalf("second close made %d attempts, want 2", got)
	}
}

func TestStopCancelsRenewalBackoff(t *testing.T) {
	bridge, old := renewalBridge(t)
	waiting := make(chan struct{})
	bridge.renewalDelay = func(ctx context.Context, _ time.Duration) error {
		close(waiting)
		<-ctx.Done()
		return ctx.Err()
	}
	var attempts atomic.Int32
	bridge.setupSession = func(*bridgeSession) error { attempts.Add(1); return nil }
	old.handleError("data_channel_close", errors.New("closed"))
	select {
	case <-waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("renewal did not begin its backoff")
	}
	bridge.Stop()
	if attempts.Load() != 0 {
		t.Fatal("session started after Stop cancelled the backoff")
	}
	if err := bridge.newSession().start(); !errors.Is(err, context.Canceled) {
		t.Fatalf("start after Stop = %v, want cancellation", err)
	}
}

func TestFailedStartKeepsForwarderUntilStop(t *testing.T) {
	bridge, _ := renewalBridge(t)
	bridge.connected = false
	bridge.setupSession = func(*bridgeSession) error { return errors.New("setup failed") }
	if err := bridge.Start(); err == nil {
		t.Fatal("failed setup reported success")
	}
	if bridge.rtpForwarder.GetClientCount() != 1 {
		t.Fatal("failed setup stopped the forwarder before Stop")
	}
	bridge.Stop()
	if bridge.rtpForwarder.GetClientCount() != 0 {
		t.Fatal("Stop did not close the forwarder")
	}
}

func TestStopWinsInFlightRenewal(t *testing.T) {
	bridge, old := renewalBridge(t)
	started := make(chan struct{})
	var attempts atomic.Int32
	bridge.setupSession = func(s *bridgeSession) error {
		attempts.Add(1)
		close(started)
		<-s.ctx.Done()
		return s.ctx.Err()
	}
	old.handleError("data_channel_close", errors.New("closed"))
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("renewal did not start")
	}
	done := make(chan struct{})
	go func() { bridge.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop blocked during renewal")
	}
	old.handleError("data_channel_close", errors.New("late close"))
	if attempts.Load() != 1 || bridge.rtpForwarder.GetClientCount() != 0 {
		t.Fatal("Stop allowed a new session or left a client")
	}
}

func TestRenewalRetriesThenClosesRTSPSessions(t *testing.T) {
	client, peer := net.Pipe()
	defer peer.Close()
	server := &RTSPServer{clients: make(map[string]*RTSPClient), streams: make(map[string]*CameraStream)}
	stream, err := server.getOrCreateStream(&storage.CameraInfo{DeviceID: "camera", DeviceName: "Camera"}, "hd", nil)
	if err != nil {
		t.Fatal(err)
	}
	rtspClient := &RTSPClient{conn: client, session: "viewer", stream: stream}
	server.addClient(rtspClient)
	stream.clients["viewer"] = rtspClient
	stream.active, stream.connecting = true, false
	bridge := stream.webrtcBridge
	t.Cleanup(bridge.Stop)
	if err := bridge.rtpForwarder.AddTCPClient("viewer", client, 0, 2, 4); err != nil {
		t.Fatal(err)
	}
	old := bridge.session.Load()
	old.isHEVC = true
	if err := old.markReady(); err != nil {
		t.Fatal(err)
	}
	bridge.connected = true
	var delays []time.Duration
	bridge.renewalDelay = func(_ context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		return nil
	}
	var attempts atomic.Int32
	bridge.setupSession = func(*bridgeSession) error {
		attempts.Add(1)
		return errors.New("unavailable")
	}
	old.handleError("data_channel_close", errors.New("closed"))
	peer.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := peer.Read(make([]byte, 1)); err == nil {
		t.Fatal("RTSP session remained open")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("RTSP session was not closed")
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
	if len(delays) != 3 || delays[0] != time.Second || delays[1] != 2*time.Second || delays[2] != 4*time.Second {
		t.Fatalf("delays = %v", delays)
	}
}

func TestNonHDAndStoppedSessionsDoNotRenew(t *testing.T) {
	for _, tt := range []struct {
		resolution string
		hevc       bool
	}{
		{"sd", true},
		{lanHDResolution, true},
		{"hd", false},
	} {
		t.Run(tt.resolution, func(t *testing.T) {
			bridge, old := renewalBridge(t)
			bridge.resolution = tt.resolution
			old.isHEVC = tt.hevc
			failed := make(chan struct{}, 1)
			bridge.OnError = func(error) { failed <- struct{}{} }
			var attempts atomic.Int32
			bridge.setupSession = func(*bridgeSession) error { attempts.Add(1); return nil }
			old.handleError("data_channel_close", errors.New("closed"))
			select {
			case <-failed:
			case <-time.After(5 * time.Second):
				t.Fatal("normal failure was not reported")
			}
			bridge.Stop()
			old.handleError("data_channel_close", errors.New("closed"))
			if attempts.Load() != 0 {
				t.Fatal("unexpected renewal")
			}
		})
	}
}

func TestHEVCPeerFailuresRenew(t *testing.T) {
	for _, source := range []string{"peer_failed", "peer_closed"} {
		t.Run(source, func(t *testing.T) {
			bridge, old := renewalBridge(t)
			bridge.setupSession = func(s *bridgeSession) error {
				s.isHEVC = true
				return nil
			}
			old.handleError(source, errors.New("failed"))
			awaitSessionReady(t, bridge, old)
		})
	}
}

func TestHEVCCloseWithoutClientsReportsFailure(t *testing.T) {
	bridge, old := renewalBridge(t)
	bridge.rtpForwarder.RemoveClient("viewer")
	failed := make(chan struct{}, 1)
	bridge.OnError = func(error) { failed <- struct{}{} }
	var attempts atomic.Int32
	bridge.setupSession = func(*bridgeSession) error { attempts.Add(1); return nil }
	old.handleError("data_channel_close", errors.New("closed"))
	select {
	case <-failed:
	case <-time.After(5 * time.Second):
		t.Fatal("failure without clients was not reported")
	}
	if attempts.Load() != 0 {
		t.Fatal("renewal started without clients")
	}
}
