package rtsp

import (
	"sync"
	"time"

	"tuya-ipc-terminal/pkg/core"
)

const speakerRetryInterval = 5 * time.Second

// Allows an in-flight on and a final off to each reach their MQTT timeouts.
var speakerOffTimeout = 5 * time.Second

// speakerState serializes commands without holding a lock during MQTT I/O.
type speakerState struct {
	mu        sync.Mutex
	send      func(int) error
	closed    bool
	abandon   bool
	talkers   map[string]struct{}
	on        bool
	uncertain bool
	lastFail  time.Time
	done      chan struct{}
}

func (s *speakerState) setSender(send func(int) error) {
	s.mu.Lock()
	s.send = send
	s.startLocked()
	s.mu.Unlock()
}

func (s *speakerState) packetForwarded(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if s.talkers == nil {
		s.talkers = make(map[string]struct{})
	}
	s.talkers[sessionID] = struct{}{}
	s.startLocked()
}

func (s *speakerState) clientLeft(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.talkers, sessionID)
	s.startLocked()
}

func (s *speakerState) startLocked() {
	if s.done != nil || s.send == nil || s.abandon {
		return
	}
	if !s.needsCommandLocked() {
		return
	}
	s.done = make(chan struct{})
	go s.run(s.done)
}

func (s *speakerState) needsCommandLocked() bool {
	if !s.closed && len(s.talkers) != 0 {
		return !s.on && time.Since(s.lastFail) >= speakerRetryInterval
	}
	return s.on || s.uncertain
}

func (s *speakerState) run(done chan struct{}) {
	for {
		s.mu.Lock()
		wantOn := !s.closed && len(s.talkers) != 0
		if s.abandon || !s.needsCommandLocked() {
			s.done = nil
			close(done)
			s.mu.Unlock()
			return
		}
		send := s.send
		s.mu.Unlock()

		value := 0
		if wantOn {
			value = 1
		}
		err := send(value)

		s.mu.Lock()
		if wantOn && err != nil {
			s.lastFail = time.Now()
			// A publish failure may still have reached the camera. Attempt an
			// off when the last talker leaves or the bridge stops.
			s.uncertain = true
		} else {
			s.on = wantOn
			s.uncertain = false
			s.lastFail = time.Time{}
		}
		if err != nil {
			core.Logger.Warn().Err(err).Msgf("Failed to set camera speaker to %d", value)
		}
		s.mu.Unlock()
	}
}

// stop prevents new commands, then waits briefly for the final off before MQTT
// is closed. If the in-flight send hangs, it must not start another afterwards.
func (s *speakerState) stop() {
	s.mu.Lock()
	s.closed = true
	s.startLocked()
	done := s.done
	s.mu.Unlock()
	if done == nil {
		return
	}
	timer := time.NewTimer(speakerOffTimeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		s.mu.Lock()
		s.abandon = true
		s.mu.Unlock()
		core.Logger.Warn().Msg("Timed out muting the camera speaker")
	}
}
