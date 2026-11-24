//go:build cgo

package zerotier

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
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
			if serr != nil {
				return
			}
			// The physical UDP socket carries the whole encrypted tunnel. Go leaves the
			// kernel buffers at the system default (tens of KB); on a lossy mobile path
			// a burst overflow silently drops tunnel packets, which shows up as TCP
			// retransmits on the far side. Ask for generous buffers (kernel clamps to
			// net.core.{r,w}mem_max) and read the granted value back.
			serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, udpRequestedRcvBuf)
			if serr == nil {
				serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF, udpRequestedSndBuf)
			}
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
	recordUDPBuffers(conn)
	return conn, nil
}

// Requested physical UDP socket buffers. The kernel clamps these to
// net.core.rmem_max / wmem_max; recordUDPBuffers reads back what was granted.
const (
	udpRequestedRcvBuf = 4 << 20 // 4 MiB
	udpRequestedSndBuf = 1 << 20 // 1 MiB
)

var (
	udpRcvBufGranted atomic.Int64
	udpSndBufGranted atomic.Int64
)

// recordUDPBuffers reads back the socket buffers the kernel actually granted
// (Linux doubles the value internally) and logs them once per bind so the
// real numbers are visible instead of assumed.
func recordUDPBuffers(conn *net.UDPConn) {
	raw, err := conn.SyscallConn()
	if err != nil {
		Warnf("[ZT] udp socket buffer readback failed: %v", err)
		return
	}
	var rcv, snd int
	_ = raw.Control(func(fd uintptr) {
		rcv, _ = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF)
		snd, _ = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF)
	})
	udpRcvBufGranted.Store(int64(rcv))
	udpSndBufGranted.Store(int64(snd))
	Infof("[ZT] udp socket buffers granted rcv=%d snd=%d (requested %d/%d)",
		rcv, snd, udpRequestedRcvBuf, udpRequestedSndBuf)
}

// UDPSocketBuffers reports the granted SO_RCVBUF / SO_SNDBUF of the physical
// ZeroTier UDP socket (0 = not bound yet).
func UDPSocketBuffers() (rcv, snd int) {
	return int(udpRcvBufGranted.Load()), int(udpSndBufGranted.Load())
}

// listenUDPReuse6 binds the IPv6 side of the physical path.
//
// Why a second socket instead of one dual-stack socket (2026-11-24):
//   - the v4 socket must stay exactly as it is (byte-for-byte path for the
//     working WiFi case: 27k frames delivered with 0 drops),
//   - a dual-stack AF_INET6 socket would change how *every* v4 address is
//     represented to the core (v4-mapped) — an unnecessary risk for the
//     working path,
//   - "udp6" sets IPV6_V6ONLY=1, so both sockets can listen on the same
//     port number without EADDRINUSE.
//
// A missing IPv6 stack is not fatal: start() only skips the v6 side.
func listenUDPReuse6(port int) (*net.UDPConn, error) {
	lc := net.ListenConfig{}
	lc.Control = func(network, address string, c syscall.RawConn) error {
		var serr error
		err := c.Control(func(fd uintptr) {
			serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
			if serr != nil {
				return
			}
			serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, udpRequestedRcvBuf)
			if serr == nil {
				serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF, udpRequestedSndBuf)
			}
		})
		if err != nil {
			return err
		}
		return serr
	}
	pc, err := lc.ListenPacket(context.Background(), "udp6", fmt.Sprintf("[::]:%d", port))
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
