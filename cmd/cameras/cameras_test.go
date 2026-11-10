package cameras

import "testing"

func TestIsCameraCategory(t *testing.T) {
	tests := []struct {
		category string
		want     bool
	}{
		{category: "sp", want: true},
		{category: "dghsxj", want: true},
		{category: "cdsxj", want: true},
		{category: "sp_wnq", want: true},
		{category: "ksdjml", want: true},
		{category: "unknown", want: false},
		{category: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.category, func(t *testing.T) {
			if got := isCameraCategory(tt.category); got != tt.want {
				t.Errorf("isCameraCategory(%q) = %t, want %t", tt.category, got, tt.want)
			}
		})
	}
}
