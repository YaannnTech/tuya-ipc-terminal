package rtsp

import (
	"errors"
	"testing"
)

func TestMatchesProcessIdentity(t *testing.T) {
	for _, tt := range []struct {
		name     string
		recorded string
		actual   string
		err      error
		want     bool
	}{
		{"matching", "one", "one", nil, true},
		{"different", "one", "two", nil, false},
		{"older record", "", "two", nil, true},
		{"unavailable", "one", "", errors.New("unavailable"), true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchesProcessIdentity(tt.recorded, tt.actual, tt.err); got != tt.want {
				t.Fatalf("matchesProcessIdentity() = %v, want %v", got, tt.want)
			}
		})
	}
}
