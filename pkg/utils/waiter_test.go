package utils

import (
	"errors"
	"testing"
)

func TestWaiterReturnsErrorFromDone(t *testing.T) {
	want := errors.New("camera client disconnected")
	for i := 0; i < 1000; i++ {
		var w Waiter
		started := make(chan struct{})
		result := make(chan error, 1)
		go func() {
			close(started)
			result <- w.Wait()
		}()
		<-started
		// Done may run before or after Wait registers; both must report err.
		w.Done(want)
		if got := <-result; !errors.Is(got, want) {
			t.Fatalf("run %d: Wait() = %v, want %v", i, got, want)
		}
	}
}
