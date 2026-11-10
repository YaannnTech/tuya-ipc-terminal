package rtsp

import "testing"

func TestStartCommandAddressDefaultsToAllInterfaces(t *testing.T) {
	flag := newStartCmd().Flags().Lookup("address")
	if flag == nil {
		t.Fatal("start command has no address flag")
	}
	if flag.DefValue != "" {
		t.Fatalf("address default = %q, want empty to preserve all-interface binding", flag.DefValue)
	}
}
