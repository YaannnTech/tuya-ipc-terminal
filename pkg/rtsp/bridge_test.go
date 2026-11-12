package rtsp

import (
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pion/rtp"
	pion "github.com/pion/webrtc/v4"

	"tuya-ipc-terminal/pkg/storage"
	"tuya-ipc-terminal/pkg/tuya"
	"tuya-ipc-terminal/pkg/utils"
)

func TestICEServerDiagnosticRedactsCredentials(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"turn:alice:secret@turn.example:3478?password=query-secret", `url="turn:turn.example:3478" ttl_raw=600`},
		{"turns://alice:secret@[2001:db8::1]:5349?transport=tcp", `url="turns:[2001:db8::1]:5349" ttl_raw=600`},
		{"stun:stun.example:3478", `url="stun:stun.example:3478" ttl_raw=600`},
		{"turn:alice:secret@bad host:3478", `url="<invalid>" ttl_raw=600`},
		{"https://alice:secret@example.com", `url="<invalid>" ttl_raw=600`},
	}
	for _, tt := range tests {
		server := tuya.ICEServer{Urls: tt.url, Username: "alice", Credential: "secret", Ttl: 600}
		if got := iceServerDiagnostic(server); got != tt.want {
			t.Errorf("diagnostic = %q, want %q", got, tt.want)
		}
	}
}

func TestRemoteCandidatesWaitForTheAnswer(t *testing.T) {
	var added []string
	queue := newRemoteCandidateQueue(func(candidate string) error {
		added = append(added, candidate)
		return nil
	})

	queue.Add("candidate-1")
	queue.Add("candidate-2")
	if len(added) != 0 {
		t.Fatalf("candidates added before the answer: %v", added)
	}

	if err := queue.SetReady(func() error {
		if len(added) != 0 {
			t.Fatalf("candidates added before the remote description was applied: %v", added)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	queue.Add("candidate-3")

	if want := []string{"candidate-1", "candidate-2", "candidate-3"}; !reflect.DeepEqual(added, want) {
		t.Fatalf("added = %v, want %v", added, want)
	}
}

func TestRemoteCandidatesStayQueuedWhenTheAnswerFails(t *testing.T) {
	var added []string
	queue := newRemoteCandidateQueue(func(candidate string) error {
		added = append(added, candidate)
		return nil
	})

	queue.Add("candidate-1")
	if err := queue.SetReady(func() error { return errors.New("bad answer") }); err == nil {
		t.Fatal("SetReady ignored the failed remote description")
	}
	queue.Add("candidate-2")

	if len(added) != 0 {
		t.Fatalf("candidates added without a remote description: %v", added)
	}
}

func newTestBridge() *WebRTCBridge {
	return NewWebRTCBridge(&storage.CameraInfo{DeviceID: "camera", DeviceName: "Camera"}, "hd", nil, nil)
}

func TestWaitForTimesOut(t *testing.T) {
	old := startTimeout
	startTimeout = 50 * time.Millisecond
	defer func() { startTimeout = old }()

	bridge := newTestBridge()
	var never utils.Waiter

	start := time.Now()
	err := bridge.session.Load().waitFor(&never, "the camera stream")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("waitFor() = %v, want a timeout error", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("waitFor took %s with a %s timeout", elapsed, startTimeout)
	}
}

func TestStopDoesNotDeadlockWhileStartIsWaiting(t *testing.T) {
	bridge := newTestBridge()
	var never utils.Waiter

	// Mimic Start: it holds the bridge lock while it waits for the camera.
	waiting := make(chan struct{})
	started := make(chan error, 1)
	go func() {
		bridge.mutex.Lock()
		defer bridge.mutex.Unlock()
		close(waiting)
		started <- bridge.session.Load().waitFor(&never, "the camera stream")
	}()
	<-waiting

	stopped := make(chan struct{})
	go func() {
		bridge.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop blocked while Start was waiting for the camera")
	}
	if err := <-started; err == nil {
		t.Fatal("the waiting Start did not fail after Stop")
	}
}

func TestStopCleansUpABridgeThatNeverConnected(t *testing.T) {
	bridge := newTestBridge()
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	if err := bridge.rtpForwarder.AddTCPClient("client", serverConn, 0, 2, 4); err != nil {
		t.Fatal(err)
	}

	bridge.Stop() // e.g. after a failed Start or a camera disconnect

	if n := bridge.rtpForwarder.GetClientCount(); n != 0 {
		t.Fatalf("forwarder still has %d client(s): Stop skipped the cleanup", n)
	}
	if bridge.ctx.Err() == nil {
		t.Fatal("bridge context was not cancelled")
	}
}

func TestFailedStartClosesClientSessions(t *testing.T) {
	clientConn, peerConn := net.Pipe()
	defer peerConn.Close()

	const sessionID = "waiting-client"
	server := &RTSPServer{clients: make(map[string]*RTSPClient), streams: make(map[string]*CameraStream)}
	stream := NewCameraStream(&storage.CameraInfo{DeviceID: "camera", DeviceName: "Camera"}, "hd", nil, nil, server)
	stream.connecting = true
	client := &RTSPClient{conn: clientConn, session: sessionID, stream: stream}
	server.addClient(client)
	stream.clients[sessionID] = client

	stream.startStreamWith(func() error { return errors.New("camera never answered") })

	// The closed connection tells the RTSP client to reconnect.
	peerConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := peerConn.Read(make([]byte, 1)); err == nil {
		t.Fatal("client connection still open after the stream failed")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("client connection was not closed after the stream failed")
	}
}

func TestBridgeErrorClosesSessionsOfAStreamWithClients(t *testing.T) {
	clientConn, peerConn := net.Pipe()
	defer peerConn.Close()

	const sessionID = "watching-client"
	server := &RTSPServer{clients: make(map[string]*RTSPClient), streams: make(map[string]*CameraStream)}
	stream, err := server.getOrCreateStream(&storage.CameraInfo{DeviceID: "camera", DeviceName: "Camera"}, "hd", nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &RTSPClient{conn: clientConn, session: sessionID, stream: stream}
	server.addClient(client)
	stream.mutex.Lock()
	stream.clients[sessionID] = client
	stream.active, stream.connecting = true, false
	stream.mutex.Unlock()

	// e.g. the camera disconnected while the client was watching
	stream.webrtcBridge.OnError(errors.New("camera client disconnected"))

	peerConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := peerConn.Read(make([]byte, 1)); err == nil {
		t.Fatal("client connection still open after the stream failed")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("a stream with a dead bridge kept its client waiting for packets")
	}
}

func TestErrorsAfterOwnShutdownAreNotReported(t *testing.T) {
	bridge := newTestBridge()
	reported := make(chan error, 2)
	bridge.OnError = func(err error) { reported <- err }
	if err := bridge.session.Load().markReady(); err != nil {
		t.Fatal(err)
	}

	bridge.session.Load().handleError("test", errors.New("data channel failed"))
	select {
	case <-reported:
	case <-time.After(5 * time.Second):
		t.Fatal("OnError was not called before shutdown")
	}

	bridge.Stop()
	// e.g. the camera confirming the disconnect that Stop sent
	bridge.session.Load().handleError("test", errors.New("camera client disconnected"))
	select {
	case <-reported:
		t.Fatalf("OnError called for an error after the bridge shut itself down")
	default:
	}
}

func TestStreamDoesNotBecomeActiveAfterStartupError(t *testing.T) {
	server := &RTSPServer{clients: make(map[string]*RTSPClient), streams: make(map[string]*CameraStream)}
	stream, err := server.getOrCreateStream(&storage.CameraInfo{DeviceID: "camera", DeviceName: "Camera"}, lanHDResolution, nil)
	if err != nil {
		t.Fatal(err)
	}
	stream.mutex.Lock()
	stream.starting = true
	stream.mutex.Unlock()

	stream.startStreamWith(func() error {
		stream.webrtcBridge.OnError(errors.New("ICE peer changed during startup"))
		return nil
	})

	stream.mutex.RLock()
	defer stream.mutex.RUnlock()
	if stream.active || stream.connecting || stream.starting {
		t.Fatalf("stream revived after startup error: active=%t connecting=%t starting=%t",
			stream.active, stream.connecting, stream.starting)
	}
}

func TestBackchannelSetAndForwardDoNotRace(t *testing.T) {
	bridge := newTestBridge()
	packet := &rtp.Packet{Header: rtp.Header{Version: 2}, Payload: []byte{1}}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 1000; i++ {
			track, err := pion.NewTrackLocalStaticRTP(
				pion.RTPCodecCapability{MimeType: pion.MimeTypeOpus},
				"audio-backchannel", "pion",
			)
			if err != nil {
				t.Error(err)
				return
			}
			bridge.session.Load().setBackchannel(track)
		}
	}()

	for i := 0; i < 1000; i++ {
		bridge.ForwardBackchannelAudioPacket("s1", packet)
	}
	<-done
}
