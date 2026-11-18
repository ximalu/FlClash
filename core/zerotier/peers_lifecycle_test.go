//go:build linux && !android && cgo

package zerotier

// Peer-plane lifecycle: the snapshot must be real while a node is alive and
// must report "unknown" (never an empty list) once it is gone. It also
// exercises the wrapper's mutex against node_delete (2026-11-18 review ⑥).

import (
	"sync"
	"testing"
	"time"
)

func TestPeerSnapshotLifecycle(t *testing.T) {
	port := pickFreePort(t)
	e, err := StartEngine(testConfig(port), nil, t.TempDir())
	if err != nil {
		t.Fatalf("StartEngine: %v", err)
	}

	peers, ok := e.PeerStatuses()
	if !ok {
		t.Fatal("PeerStatuses must succeed while the engine is RUNNING")
	}
	if peers == nil {
		t.Fatal("peers must be an empty slice, not nil, when the query ran")
	}
	t.Logf("live peers right after start: %d", len(peers))

	status := GetRuntimeStatus()
	if status.UpdatedAt == 0 {
		t.Fatal("updatedAt missing")
	}
	if status.UDPPort == nil || *status.UDPPort != port {
		t.Fatalf("udpPort = %v, want %d", status.UDPPort, port)
	}
	if status.SocketGen == nil {
		t.Fatal("socketGen missing")
	}
	if status.SelfHealAttempts == nil || status.SelfHealSuccesses == nil || status.SelfHealMax == nil {
		t.Fatalf("self-heal stats missing: %+v", status)
	}
	if *status.SelfHealMax != socketFixMax {
		t.Fatalf("selfHealMax = %d, want %d", *status.SelfHealMax, socketFixMax)
	}
	if status.LinkHealth == "" {
		t.Fatal("linkHealth must always carry a value")
	}
	if status.Peers == nil {
		t.Fatal("status.Peers must be non-nil while running")
	}

	// Concurrent snapshot vs teardown: serialized on the node mutex, so it
	// must neither crash nor deadlock.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			_, _ = e.PeerStatuses()
			time.Sleep(2 * time.Millisecond)
		}
	}()
	time.Sleep(50 * time.Millisecond)
	e.Stop()
	wg.Wait()

	if _, ok := e.PeerStatuses(); ok {
		t.Fatal("PeerStatuses must fail after the node is gone")
	}
	if status := GetRuntimeStatus(); status.Peers != nil {
		t.Fatalf("status.Peers after stop = %v, want nil (unknown)", status.Peers)
	}
}
