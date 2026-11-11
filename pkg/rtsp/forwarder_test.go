package rtsp

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"tuya-ipc-terminal/pkg/storage"

	"github.com/pion/rtp"
)

func testPacket() *rtp.Packet {
	return &rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: 1, Timestamp: 1, SSRC: 1},
		Payload: []byte{1, 2, 3},
	}
}

func waitForDeadClient(t *testing.T, dead <-chan string, want string) {
	t.Helper()
	select {
	case got := <-dead:
		if got != want {
			t.Fatalf("OnDeadClient(%q), want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnDeadClient was not called")
	}
}

func TestForwarderDropsClosedTCPClient(t *testing.T) {
	t.Setenv("TUYA_VIDEO_TS_DIVISOR", "1")
	clientConn, serverConn := net.Pipe()
	clientConn.Close()
	defer serverConn.Close()

	forwarder := NewRTPForwarder()
	dead := make(chan string, 1)
	forwarder.OnDeadClient = func(sessionID string) { dead <- sessionID }
	if err := forwarder.AddTCPClient("gone", serverConn, 0, 2, 4); err != nil {
		t.Fatal(err)
	}

	forwarder.ForwardVideoPacket(testPacket())

	waitForDeadClient(t, dead, "gone")
	if n := forwarder.GetClientCount(); n != 0 {
		t.Fatalf("client count = %d, want 0 after the client was dropped", n)
	}
}

func TestForwarderDropsStalledTCPClientWithoutBlockingOthers(t *testing.T) {
	t.Setenv("TUYA_VIDEO_TS_DIVISOR", "1")
	old := tcpWriteTimeout
	tcpWriteTimeout = 100 * time.Millisecond
	defer func() { tcpWriteTimeout = old }()

	stalledClient, stalledServer := net.Pipe() // nobody reads stalledClient
	defer stalledClient.Close()
	defer stalledServer.Close()
	liveClient, liveServer := net.Pipe()
	defer liveClient.Close()
	defer liveServer.Close()

	forwarder := NewRTPForwarder()
	dead := make(chan string, 1)
	forwarder.OnDeadClient = func(sessionID string) { dead <- sessionID }
	if err := forwarder.AddTCPClient("stalled", stalledServer, 0, 2, 4); err != nil {
		t.Fatal(err)
	}
	if err := forwarder.AddTCPClient("live", liveServer, 0, 2, 4); err != nil {
		t.Fatal(err)
	}

	go io.Copy(io.Discard, liveClient)
	done := make(chan struct{})
	go func() {
		forwarder.ForwardVideoPacket(testPacket())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("forwarding blocked on a client that stopped reading")
	}
	waitForDeadClient(t, dead, "stalled")
	if n := forwarder.GetClientCount(); n != 1 {
		t.Fatalf("client count = %d, want only the live client left", n)
	}
}

func TestInterleavedWriteClearsDeadline(t *testing.T) {
	old := tcpWriteTimeout
	tcpWriteTimeout = 50 * time.Millisecond
	defer func() { tcpWriteTimeout = old }()

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	go io.Copy(io.Discard, clientConn)

	forwarder := NewRTPForwarder()
	if err := forwarder.sendInterleavedRTP(serverConn, 0, []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * tcpWriteTimeout)

	// An RTSP response written later on the same connection must not hit the
	// deadline set for the RTP write.
	if _, err := serverConn.Write([]byte("RTSP/1.0 200 OK\r\n\r\n")); err != nil {
		t.Fatalf("write after RTP forwarding failed: %v", err)
	}
}

func TestForwarderDropsUDPClientWithClosedPort(t *testing.T) {
	t.Setenv("TUYA_VIDEO_TS_DIVISOR", "1")
	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	port := listener.LocalAddr().(*net.UDPAddr).Port
	listener.Close() // the port now answers with ICMP port unreachable

	forwarder := NewRTPForwarder()
	dead := make(chan string, 1)
	forwarder.OnDeadClient = func(sessionID string) { dead <- sessionID }
	if err := forwarder.AddUDPClient("gone", "127.0.0.1", port, 0); err != nil {
		t.Fatal(err)
	}

	// The refusal is reported on the send after the ICMP message arrives.
	for i := 0; i < 50 && forwarder.GetClientCount() > 0; i++ {
		forwarder.ForwardVideoPacket(testPacket())
		time.Sleep(10 * time.Millisecond)
	}
	waitForDeadClient(t, dead, "gone")
}

func TestForwarderStopIsRaceSafeWithClientChanges(t *testing.T) {
	forwarder := NewRTPForwarder()
	var conns []net.Conn
	for i := 0; i < 20; i++ {
		a, b := net.Pipe()
		conns = append(conns, a, b)
		_ = forwarder.AddTCPClient(string(rune('a'+i)), b, 0, 2, 4)
	}
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		forwarder.Stop()
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			a, b := net.Pipe()
			defer a.Close()
			defer b.Close()
			_ = forwarder.AddTCPClient(string(rune('A'+i)), b, 0, 2, 4)
		}
	}()
	wg.Wait()
}

func TestDeadRTPClientClosesItsRTSPSession(t *testing.T) {
	t.Setenv("TUYA_VIDEO_TS_DIVISOR", "1")
	clientConn, peerConn := net.Pipe()
	defer peerConn.Close()

	const sessionID = "dead-rtp-client"
	server := &RTSPServer{clients: make(map[string]*RTSPClient)}
	stream := NewCameraStream(&storage.CameraInfo{DeviceID: "camera"}, "hd", nil, nil, server)
	client := &RTSPClient{conn: clientConn, session: sessionID, stream: stream}
	server.addClient(client)
	stream.clients[sessionID] = client // AddClient would start a real Tuya connection

	// peerConn is never read, so the closed RTSP connection shows up as EOF here.
	peerConn.Close()
	if err := stream.webrtcBridge.rtpForwarder.AddTCPClient(sessionID, clientConn, 0, 2, 4); err != nil {
		t.Fatal(err)
	}
	stream.webrtcBridge.rtpForwarder.ForwardVideoPacket(testPacket())

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		server.mutex.RLock()
		_, inServer := server.clients[sessionID]
		server.mutex.RUnlock()
		stream.mutex.RLock()
		_, inStream := stream.clients[sessionID]
		stream.mutex.RUnlock()
		if !inServer && !inStream {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the RTSP session of a dead RTP client was not removed from the server and stream")
}
