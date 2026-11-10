package rtsp

import (
	"bufio"
	"bytes"
	"errors"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	pion "github.com/pion/webrtc/v4"
)

type speakerRecorder struct {
	mu    sync.Mutex
	calls []int
	err   error
}

func (r *speakerRecorder) send(value int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, value)
	return r.err
}

func (r *speakerRecorder) setErr(err error) {
	r.mu.Lock()
	r.err = err
	r.mu.Unlock()
}

func (r *speakerRecorder) got() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.calls...)
}

func expectCalls(t *testing.T, r *speakerRecorder, want ...int) {
	t.Helper()
	got := r.got()
	if len(got) != len(want) {
		t.Fatalf("SendSpeaker calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("SendSpeaker calls = %v, want %v", got, want)
		}
	}
}

func waitSpeaker(t *testing.T, s *speakerState) {
	t.Helper()
	s.mu.Lock()
	done := s.done
	s.mu.Unlock()
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("speaker command did not finish")
	}
}

func newTalkbackBridge(t *testing.T) (*WebRTCBridge, *speakerRecorder) {
	t.Helper()
	bridge := newTestBridge()
	track, err := pion.NewTrackLocalStaticRTP(
		pion.RTPCodecCapability{MimeType: pion.MimeTypeOpus},
		"audio-backchannel", "pion",
	)
	if err != nil {
		t.Fatal(err)
	}
	bridge.setBackchannel(track)
	rec := &speakerRecorder{}
	bridge.speaker.setSender(rec.send)
	return bridge, rec
}

func talkPacket() *rtp.Packet {
	return &rtp.Packet{Header: rtp.Header{Version: 2}, Payload: []byte{1}}
}

func TestSpeakerTurnedOnOnceUnderConcurrency(t *testing.T) {
	bridge, rec := newTalkbackBridge(t)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(session string) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				bridge.ForwardBackchannelAudioPacket(session, talkPacket())
			}
		}(string(rune('a' + i)))
	}
	wg.Wait()

	waitSpeaker(t, &bridge.speaker)
	expectCalls(t, rec, 1)
	for i := 0; i < 3; i++ {
		bridge.ForwardBackchannelAudioPacket("a", talkPacket())
		waitSpeaker(t, &bridge.speaker)
	}
	expectCalls(t, rec, 1)
}

func TestSpeakerNotTurnedOnWithoutBackchannel(t *testing.T) {
	bridge := newTestBridge()
	rec := &speakerRecorder{}
	bridge.speaker.setSender(rec.send)

	bridge.ForwardBackchannelAudioPacket("a", talkPacket())

	waitSpeaker(t, &bridge.speaker)
	expectCalls(t, rec)
}

func TestSpeakerRetriesAfterFailure(t *testing.T) {
	bridge, rec := newTalkbackBridge(t)
	rec.setErr(errors.New("mqtt down"))

	bridge.ForwardBackchannelAudioPacket("a", talkPacket())
	waitSpeaker(t, &bridge.speaker)
	bridge.ForwardBackchannelAudioPacket("a", talkPacket())
	waitSpeaker(t, &bridge.speaker)
	expectCalls(t, rec, 1)
	for i := 0; i < 20; i++ {
		bridge.ForwardBackchannelAudioPacket("a", talkPacket())
	}
	waitSpeaker(t, &bridge.speaker)
	expectCalls(t, rec, 1)

	rec.setErr(nil)
	bridge.speaker.mu.Lock()
	bridge.speaker.lastFail = time.Now().Add(-speakerRetryInterval)
	bridge.speaker.mu.Unlock()
	bridge.ForwardBackchannelAudioPacket("a", talkPacket())
	bridge.ForwardBackchannelAudioPacket("a", talkPacket())
	waitSpeaker(t, &bridge.speaker)
	expectCalls(t, rec, 1, 1)
}

func TestSpeakerTurnedOffWhenLastClientLeaves(t *testing.T) {
	bridge, rec := newTalkbackBridge(t)

	bridge.ForwardBackchannelAudioPacket("a", talkPacket())
	bridge.ForwardBackchannelAudioPacket("b", talkPacket())
	waitSpeaker(t, &bridge.speaker)
	expectCalls(t, rec, 1)

	bridge.speaker.clientLeft("c") // never talked
	bridge.speaker.clientLeft("a")
	waitSpeaker(t, &bridge.speaker)
	expectCalls(t, rec, 1)

	bridge.speaker.clientLeft("b")
	waitSpeaker(t, &bridge.speaker)
	expectCalls(t, rec, 1, 0)

	bridge.speaker.clientLeft("b")
	waitSpeaker(t, &bridge.speaker)
	expectCalls(t, rec, 1, 0)
}

func TestSpeakerClientRemovedFromForwarderTurnsItOff(t *testing.T) {
	bridge, rec := newTalkbackBridge(t)

	bridge.rtpForwarder.mutex.Lock()
	bridge.rtpForwarder.clients["a"] = &RTPClient{sessionID: "a", transportMode: TransportTCP}
	bridge.rtpForwarder.mutex.Unlock()

	bridge.ForwardBackchannelAudioPacket("a", talkPacket())
	waitSpeaker(t, &bridge.speaker)
	bridge.rtpForwarder.RemoveClient("a")

	waitSpeaker(t, &bridge.speaker)
	expectCalls(t, rec, 1, 0)
}

func TestSpeakerTurnedOffOnStopOnly(t *testing.T) {
	bridge, rec := newTalkbackBridge(t)

	bridge.closeResources() // never talked: no command
	expectCalls(t, rec)

	bridge, rec = newTalkbackBridge(t)
	bridge.ForwardBackchannelAudioPacket("a", talkPacket())
	waitSpeaker(t, &bridge.speaker)
	bridge.closeResources()
	expectCalls(t, rec, 1, 0)

	bridge.ForwardBackchannelAudioPacket("a", talkPacket())
	bridge.speaker.clientLeft("a")
	waitSpeaker(t, &bridge.speaker)
	expectCalls(t, rec, 1, 0)
}

func TestSpeakerStopDoesNotBlockOnStuckSend(t *testing.T) {
	old := speakerOffTimeout
	speakerOffTimeout = 50 * time.Millisecond
	defer func() { speakerOffTimeout = old }()

	bridge, _ := newTalkbackBridge(t)
	release := make(chan struct{})
	started := make(chan struct{})
	bridge.ForwardBackchannelAudioPacket("a", talkPacket())
	waitSpeaker(t, &bridge.speaker)
	bridge.speaker.setSender(func(int) error { close(started); <-release; return nil })
	bridge.speaker.clientLeft("a")
	<-started

	done := make(chan struct{})
	go func() {
		bridge.speaker.stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		close(release)
		<-done
		t.Fatal("stop blocked on a stuck SendSpeaker")
	}
	close(release)
	waitSpeaker(t, &bridge.speaker)
}

func TestSpeakerSerializesOffAndNewTalker(t *testing.T) {
	bridge, rec := newTalkbackBridge(t)
	offStarted := make(chan struct{})
	releaseOff := make(chan struct{})
	bridge.speaker.setSender(func(value int) error {
		if value == 0 {
			close(offStarted)
			<-releaseOff
		}
		return rec.send(value)
	})
	bridge.ForwardBackchannelAudioPacket("a", talkPacket())
	waitSpeaker(t, &bridge.speaker)
	bridge.speaker.clientLeft("a")
	<-offStarted
	bridge.speaker.mu.Lock()
	offDone := bridge.speaker.done
	bridge.speaker.mu.Unlock()
	bridge.ForwardBackchannelAudioPacket("b", talkPacket())
	bridge.speaker.mu.Lock()
	sameWorker := offDone != nil && bridge.speaker.done == offDone
	bridge.speaker.mu.Unlock()
	close(releaseOff)
	waitSpeaker(t, &bridge.speaker)
	if !sameWorker {
		t.Fatal("speaker command worker changed while off was in flight")
	}
	expectCalls(t, rec, 1, 0, 1)
}

func TestSpeakerStopWaitsForInFlightOn(t *testing.T) {
	bridge, rec := newTalkbackBridge(t)
	if err := bridge.rtpForwarder.AddTCPClient("a", nil, 0, 2, 4); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	bridge.speaker.setSender(func(value int) error {
		if value == 1 {
			close(started)
			<-release
		}
		return rec.send(value)
	})
	bridge.rtpForwarder.forwardBackchannel("a", talkPacket())
	<-started
	removed := make(chan struct{})
	go func() {
		bridge.rtpForwarder.RemoveClient("a")
		close(removed)
	}()
	select {
	case <-removed:
	case <-time.After(2 * time.Second):
		close(release)
		<-removed
		t.Fatal("client removal blocked on MQTT")
	}
	stopped := make(chan struct{})
	go func() {
		bridge.speaker.stop()
		close(stopped)
	}()
	// Observe the closed state before allowing the pending command to finish.
	deadline := time.Now().Add(4 * time.Second)
	for {
		bridge.speaker.mu.Lock()
		closed := bridge.speaker.closed
		bridge.speaker.mu.Unlock()
		if closed {
			break
		}
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("stop did not mark the speaker closed")
		}
		select {
		case <-stopped:
			close(release)
			waitSpeaker(t, &bridge.speaker)
			t.Fatal("stop returned before the in-flight command completed")
		default:
			runtime.Gosched()
		}
	}
	select {
	case <-stopped:
		close(release)
		waitSpeaker(t, &bridge.speaker)
		t.Fatal("stop returned before the in-flight command completed")
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	<-stopped
	expectCalls(t, rec, 1, 0)
}

func TestSpeakerFailedOnIsMutedWhenClientLeaves(t *testing.T) {
	bridge, rec := newTalkbackBridge(t)
	rec.setErr(errors.New("uncertain publish"))
	bridge.ForwardBackchannelAudioPacket("a", talkPacket())
	waitSpeaker(t, &bridge.speaker)
	rec.setErr(nil)
	bridge.speaker.clientLeft("a")
	waitSpeaker(t, &bridge.speaker)
	expectCalls(t, rec, 1, 0)
}

func TestSpeakerRemovedClientCannotRestartTalkback(t *testing.T) {
	bridge, rec := newTalkbackBridge(t)
	if err := bridge.rtpForwarder.AddTCPClient("a", nil, 0, 2, 4); err != nil {
		t.Fatal(err)
	}
	bridge.rtpForwarder.forwardBackchannel("a", talkPacket())
	waitSpeaker(t, &bridge.speaker)
	bridge.rtpForwarder.RemoveClient("a")
	waitSpeaker(t, &bridge.speaker)
	bridge.rtpForwarder.forwardBackchannel("a", talkPacket())
	waitSpeaker(t, &bridge.speaker)
	expectCalls(t, rec, 1, 0)
}

func TestSpeakerAbandonsTimedOutInFlightOn(t *testing.T) {
	old := speakerOffTimeout
	speakerOffTimeout = 50 * time.Millisecond
	defer func() { speakerOffTimeout = old }()

	bridge, rec := newTalkbackBridge(t)
	started := make(chan struct{})
	release := make(chan struct{})
	bridge.speaker.setSender(func(value int) error {
		if value == 1 {
			close(started)
			<-release
		}
		return rec.send(value)
	})
	bridge.ForwardBackchannelAudioPacket("a", talkPacket())
	<-started
	bridge.speaker.stop()
	close(release)
	waitSpeaker(t, &bridge.speaker)
	expectCalls(t, rec, 1)
}

func TestSpeakerDeadClientAndInactiveCleanup(t *testing.T) {
	bridge, rec := newTalkbackBridge(t)
	for _, mode := range []TransportMode{TransportTCP, TransportUDP} {
		bridge.rtpForwarder.mutex.Lock()
		bridge.rtpForwarder.clients["a"] = &RTPClient{sessionID: "a", transportMode: mode}
		bridge.rtpForwarder.mutex.Unlock()
		bridge.rtpForwarder.forwardBackchannel("a", talkPacket())
		waitSpeaker(t, &bridge.speaker)
		if mode == TransportTCP {
			bridge.rtpForwarder.dropDeadClients([]string{"a"}, "video", []error{errors.New("closed")})
		} else {
			bridge.rtpForwarder.CleanupInactiveClients(time.Second)
		}
		waitSpeaker(t, &bridge.speaker)
	}
	expectCalls(t, rec, 1, 0, 1, 0)
}

func TestSpeakerBackchannelTransports(t *testing.T) {
	for _, transport := range []string{"tcp", "udp"} {
		t.Run(transport, func(t *testing.T) {
			bridge, rec := newTalkbackBridge(t)
			t.Cleanup(bridge.rtpForwarder.Stop)
			calls := make(chan int, 2)
			bridge.speaker.setSender(func(value int) error {
				err := rec.send(value)
				calls <- value
				return err
			})
			packet, err := talkPacket().Marshal()
			if err != nil {
				t.Fatal(err)
			}
			if transport == "tcp" {
				if err := bridge.rtpForwarder.AddTCPClient("a", nil, 0, 2, 4); err != nil {
					t.Fatal(err)
				}
				frame := append([]byte{'$', 4, byte(len(packet) >> 8), byte(len(packet))}, packet...)
				client := &RTSPClient{
					session: "a", backAudioRTPChannel: 4, reader: bufio.NewReader(bytes.NewReader(frame)),
					stream: &CameraStream{webrtcBridge: bridge},
				}
				if err := (&RTSPServer{}).handleInterleavedRTP(client); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := bridge.rtpForwarder.AddUDPClient("a", "127.0.0.1", 0, 0); err != nil {
					t.Fatal(err)
				}
				port, err := bridge.rtpForwarder.SetupUDPBackchannel("a", 1234)
				if err != nil {
					t.Fatal(err)
				}
				conn, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				if _, err := conn.Write(packet); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case got := <-calls:
				if got != 1 {
					t.Fatalf("first command = %d, want on", got)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("backchannel did not turn speaker on")
			}
			bridge.rtpForwarder.RemoveClient("a")
			waitSpeaker(t, &bridge.speaker)
			expectCalls(t, rec, 1, 0)
		})
	}
}
