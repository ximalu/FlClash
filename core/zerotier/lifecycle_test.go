//go:build linux && !android && cgo

package zerotier

// P0-5 lifecycle tests (2026-08-19). These exercise the Engine state machine
// and port-binding invariants on Linux (links libzerotiercore-linux.a).
//
// Test A: normal start (STOPPED → STARTING → RUNNING), one node, one socket,
//         configured port.
// Test B: repeated Start while RUNNING → idempotent, no second node/socket.
// Test C: normal Stop (RUNNING → STOPPING → STOPPED), goroutines exit,
//         socket closed, node released.
// Test D: rapid Stop → Start cycles (100x), port stays identical, no
//         goroutine leak, no EADDRINUSE fallback.
// Test E: port occupied by another socket → START FAILED (never random port).
// Test F: VPN lifecycle stress — Start/Stop/Start/Stop 100x, track
//         goroutines / bound port / errors.

import (
	"net"
	"runtime"
	"testing"
	"time"
)

const (
	testNWID = "b6079f73c6c0eb31" // 家里 (same as real config; join is local, no network needed)
)

func testConfig(port int) Config {
	return Config{NetworkID: testNWID, Port: port}
}

func countGoroutines() int { return runtime.NumGoroutine() }

// pickFreePort asks the kernel for a free UDP port and closes it, so the
// engine can bind it. There is a small race (another process could grab it)
// but it is fine for tests.
func pickFreePort(t *testing.T) int {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		t.Fatalf("pickFreePort: %v", err)
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port
	conn.Close()
	return port
}

// TestA_NormalStart: STOPPED → Start → RUNNING. One node, one socket,
// configured port.
func TestA_NormalStart(t *testing.T) {
	port := pickFreePort(t)
	e, err := StartEngine(testConfig(port), nil, t.TempDir())
	if err != nil {
		t.Fatalf("StartEngine failed: %v", err)
	}
	if got := e.getState(); got != StateRunning {
		t.Fatalf("state after start = %v, want RUNNING", got)
	}
	if got := e.boundPort(); got != port {
		t.Fatalf("bound port = %d, want %d", got, port)
	}
	if e.nodeAddress() == 0 {
		t.Fatal("node address = 0, expected non-zero")
	}
	e.Stop()
	if got := e.getState(); got != StateStopped {
		t.Fatalf("state after stop = %v, want STOPPED", got)
	}
}

// TestB_RepeatedStartWhileRunning: RUNNING → Start → same engine, no second
// node/socket/bind.
func TestB_RepeatedStartWhileRunning(t *testing.T) {
	port := pickFreePort(t)
	e1, err := StartEngine(testConfig(port), nil, t.TempDir())
	if err != nil {
		t.Fatalf("first StartEngine failed: %v", err)
	}
	addr1 := e1.nodeAddress()
	port1 := e1.boundPort()

	e2, err := StartEngine(testConfig(port), nil, t.TempDir())
	if err != nil {
		t.Fatalf("second StartEngine failed: %v", err)
	}
	if e2 != e1 {
		t.Fatal("second Start returned a different engine; expected idempotent same engine")
	}
	if got := e2.nodeAddress(); got != addr1 {
		t.Fatalf("node address changed on idempotent start: %d -> %d", addr1, got)
	}
	if got := e2.boundPort(); got != port1 {
		t.Fatalf("bound port changed on idempotent start: %d -> %d", port1, got)
	}
	e1.Stop()
}

// TestC_NormalStop: RUNNING → Stop → STOPPED. goroutines exit, socket closed,
// node released.
func TestC_NormalStop(t *testing.T) {
	port := pickFreePort(t)
	e, err := StartEngine(testConfig(port), nil, t.TempDir())
	if err != nil {
		t.Fatalf("StartEngine failed: %v", err)
	}
	before := countGoroutines()
	e.Stop()
	// Give the driver goroutines a moment to unwind, then verify they exited.
	deadline := time.Now().Add(2 * time.Second)
	for countGoroutines() > before-3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	// 3 driver goroutines must be gone (receiveLoop, backgroundLoop, engineLoop).
	if got := countGoroutines(); got > before-3 {
		t.Fatalf("goroutines after stop = %d (before=%d), expected at least 3 exited", got, before)
	}
	// Node must be released: StartEngine should be able to create a fresh one.
	e2, err := StartEngine(testConfig(port), nil, t.TempDir())
	if err != nil {
		t.Fatalf("StartEngine after Stop failed: %v", err)
	}
	if e2 == e {
		t.Fatal("StartEngine after Stop returned the old engine; expected a fresh one")
	}
	e2.Stop()
}

// TestD_RapidStopStart: Start → Stop → immediate Start, 100x. Port stays
// identical, no EADDRINUSE fallback, no goroutine leak.
func TestD_RapidStopStart(t *testing.T) {
	port := pickFreePort(t)
	base := countGoroutines()
	for i := 0; i < 100; i++ {
		e, err := StartEngine(testConfig(port), nil, t.TempDir())
		if err != nil {
			t.Fatalf("iter %d StartEngine failed: %v", i, err)
		}
		if got := e.boundPort(); got != port {
			t.Fatalf("iter %d bound port = %d, want %d (endpoint drifted!)", i, got, port)
		}
		e.Stop()
	}
	deadline := time.Now().Add(3 * time.Second)
	for countGoroutines() > base+1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := countGoroutines(); got > base+1 {
		t.Fatalf("goroutine leak: base=%d after=%d", base, got)
	}
}

// TestE_PortOccupied: occupy the configured port with another UDP socket,
// StartEngine MUST fail — never a random-port success.
func TestE_PortOccupied(t *testing.T) {
	port := pickFreePort(t)
	holder, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: port})
	if err != nil {
		t.Fatalf("holder bind failed: %v", err)
	}
	defer holder.Close()

	e, err := StartEngine(testConfig(port), nil, t.TempDir())
	if err == nil {
		e.Stop()
		t.Fatalf("StartEngine succeeded on occupied port %d — P0-1 invariant violated (random fallback?)", port)
	}
	t.Logf("expected failure received: %v", err)
	// Global slot must be released after a failed start.
	waitGlobalEngine()
}

// TestF_VPNLifecycleStress: Start/Stop/Start/Stop 100x tracking goroutines,
// bound port, errors (overlaps TestD but keeps a separate record for the
// report).
func TestF_VPNLifecycleStress(t *testing.T) {
	port := pickFreePort(t)
	base := countGoroutines()
	var failures []string
	for i := 0; i < 100; i++ {
		e, err := StartEngine(testConfig(port), nil, t.TempDir())
		if err != nil {
			failures = append(failures, err.Error())
			continue
		}
		if got := e.boundPort(); got != port {
			failures = append(failures, "port drift")
		}
		e.Stop()
	}
	if len(failures) > 0 {
		t.Fatalf("%d failures out of 100: %v", len(failures), failures[:min(5, len(failures))])
	}
	deadline := time.Now().Add(3 * time.Second)
	for countGoroutines() > base+1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := countGoroutines(); got > base+1 {
		t.Fatalf("goroutine leak after stress: base=%d after=%d", base, got)
	}
}
