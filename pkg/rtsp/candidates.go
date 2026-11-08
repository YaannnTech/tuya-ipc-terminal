package rtsp

import (
	"sync"

	"tuya-ipc-terminal/pkg/core"
)

// remoteCandidateQueue holds the camera's ICE candidates until its answer has
// been applied. Tuya delivers answer and candidates as separate MQTT messages,
// and a candidate can arrive first; adding it then fails in pion
// ("remote description is not set") and used to abort the stream start.
type remoteCandidateQueue struct {
	mu      sync.Mutex
	ready   bool
	pending []string
	add     func(candidate string) error
}

func newRemoteCandidateQueue(add func(candidate string) error) *remoteCandidateQueue {
	return &remoteCandidateQueue{add: add}
}

// Add adds the candidate now, or queues it until SetReady succeeds.
func (q *remoteCandidateQueue) Add(candidate string) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if !q.ready {
		core.Logger.Trace().Msgf("Queueing ICE candidate until the answer is applied: %s", candidate)
		q.pending = append(q.pending, candidate)
		return
	}
	_ = q.add(candidate)
}

// SetReady applies the remote description and then adds the queued
// candidates in arrival order. Holding the lock throughout means no
// candidate can slip in between.
func (q *remoteCandidateQueue) SetReady(applyRemoteDescription func() error) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	if err := applyRemoteDescription(); err != nil {
		return err
	}

	q.ready = true
	for _, candidate := range q.pending {
		_ = q.add(candidate)
	}
	q.pending = nil
	return nil
}
