//go:build cgo

package zerotier

import "testing"

func TestGetRuntimeStatusWithoutEngine(t *testing.T) {
	globalEngineMu.Lock()
	oldEngine := globalEngine
	oldDone := globalDone
	globalEngine = nil
	globalDone = nil
	globalEngineMu.Unlock()
	defer func() {
		globalEngineMu.Lock()
		globalEngine = oldEngine
		globalDone = oldDone
		globalEngineMu.Unlock()
	}()

	status := GetRuntimeStatus()
	if status.State != StateStopped.String() {
		t.Fatalf("state = %q, want %q", status.State, StateStopped.String())
	}
	if status.NodeAddress != "" || status.IPv4 != "" || status.Routes != 0 {
		t.Fatalf("unexpected stopped status: %+v", status)
	}
}

func TestFormatNodeAddress(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   uint64
		want string
	}{
		{name: "zero", in: 0, want: ""},
		{name: "short", in: 0x1234, want: "0000000000001234"},
		{name: "full", in: 0xffffffffffffffff, want: "ffffffffffffffff"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatNodeAddress(tc.in); got != tc.want {
				t.Fatalf("formatNodeAddress(%x) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
