package rtsp

import (
	"net"
	"testing"
	"time"

	"tuya-ipc-terminal/pkg/storage"
)

func TestFindCameraByRTSPPathIgnoresCaseWhenNeeded(t *testing.T) {
	cameras := []storage.CameraInfo{{RTSPPath: "/Keller"}}

	if got := findCameraByRTSPPath(cameras, "/keller"); got != &cameras[0] {
		t.Fatalf("case-insensitive path lookup returned %p, want %p", got, &cameras[0])
	}
}

func TestFindCameraByRTSPPathPrefersExactMatch(t *testing.T) {
	cameras := []storage.CameraInfo{{RTSPPath: "/KELLER"}, {RTSPPath: "/Keller"}}

	if got := findCameraByRTSPPath(cameras, "/Keller"); got != &cameras[1] {
		t.Fatalf("path lookup returned %p, want exact match %p", got, &cameras[1])
	}
}

func TestRemoveClientDoesNotHoldServerLockDuringStreamCleanup(t *testing.T) {
	stream := &CameraStream{clients: make(map[string]*RTSPClient)}
	stream.mutex.Lock()
	defer stream.mutex.Unlock()

	clientConn, peerConn := net.Pipe()
	defer peerConn.Close()

	const sessionID = "blocked-stream-client"
	server := &RTSPServer{
		clients: map[string]*RTSPClient{
			sessionID: {conn: clientConn, stream: stream},
		},
	}
	removed := make(chan struct{})
	go func() {
		server.removeClient(sessionID)
		close(removed)
	}()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if server.mutex.TryLock() {
			_, exists := server.clients[sessionID]
			server.mutex.Unlock()
			if !exists {
				server.addClient(&RTSPClient{session: "unrelated-client"})
				return
			}
		}
		time.Sleep(time.Millisecond)
	}

	t.Fatal("client cleanup kept the server lock while waiting for the stream lock")
}
