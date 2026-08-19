//go:build cgo

package zerotier

import (
	"context"
	"fmt"
	"net"
	"syscall"
	"time"
)

// bindPort binds the configured/default UDP port with retries.
//
// P0-1 invariant: we NEVER fall back to a random port. The endpoint is a
// contract with every ZeroTier peer — controller, planets, and members all
// learn our (IP, port) path. Silently changing the port breaks path learning
// and leaves the node unreachable (observed 2026-08-19: i3 saw the phone node
// as RELAY -1 with lat=-1 after a bind failure fell back to an ephemeral
// port). If the configured port cannot be bound after bindRetries attempts,
// the engine start FAILS and the caller (FlClashTier runtime) decides whether
// to fall back to plain mihomo.
//
// SO_REUSEADDR is set explicitly to match the official ZeroTier Android
// client (ZerotierFix NodeRuntimeCore.java: socket.setReuseAddress(true)).
// P0-4 empirical note (tested on Linux 6.8):
//   - Go net.ListenUDP does NOT set SO_REUSEADDR by default (defaults:
//     SO_REUSEADDR=0 SO_REUSEPORT=0 SO_BROADCAST=1).
//   - For UDP, SO_REUSEADDR does NOT allow a second bind while the old
//     socket is still open (verified: EADDRINUSE). The real fix for that
//     race is lifecycle ordering (Stop closes the socket before wg.Wait,
//     and StartEngine waits for a STOPPING engine to finish).
//   - UDP has no TIME_WAIT, so once the old socket is closed the port is
//     immediately reusable (verified). SO_REUSEADDR mainly matters for TCP
//     TIME_WAIT and multicast; setting it here is harmless and aligns with
//     upstream, but it is NOT the mechanism that prevents EADDRINUSE.
func bindPort(port int) (*net.UDPConn, error) {
	if port <= 0 || port > 65535 {
		return nil, fmt.Errorf("invalid UDP port %d", port)
	}
	var lastErr error
	for i := 0; i < bindRetries; i++ {
		conn, err := listenUDPReuse(port)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		// A closing socket (previous engine still tearing down) can take a
		// moment to release the port. Retry briefly instead of giving up or
		// jumping to a random port.
		time.Sleep(bindRetryWait)
	}
	return nil, lastErr
}

// listenUDPReuse creates a UDP socket with SO_REUSEADDR set, bound to the
// given port. Explicit Control hook because Go's net.ListenUDP leaves
// SO_REUSEADDR off by default (verified on Linux).
func listenUDPReuse(port int) (*net.UDPConn, error) {
	lc := net.ListenConfig{}
	lc.Control = func(network, address string, c syscall.RawConn) error {
		var serr error
		err := c.Control(func(fd uintptr) {
			serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
		})
		if err != nil {
			return err
		}
		return serr
	}
	pc, err := lc.ListenPacket(context.Background(), "udp4", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		return nil, err
	}
	conn, ok := pc.(*net.UDPConn)
	if !ok {
		pc.Close()
		return nil, fmt.Errorf("unexpected packet conn type %T", pc)
	}
	return conn, nil
}
