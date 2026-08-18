//go:build cgo

// Engine: Go-side ZeroTier node driver. Owns the node handle, the protected
// physical UDP socket, and the three driver loops:
//
//	receiveLoop   — UDP recv → processWirePacket (wire in)
//	backgroundLoop — deadline-driven processBackgroundTasks (timeouts/retries)
//	engineLoop     — frame queue pull + config snapshot refresh
//
// Lifecycle order (learned the hard way in M1-3): close(stopCh) → wg.Wait()
// (no goroutine inside the C core) → close socket → leave → delete node.
package zerotier

/*
#include "wrapper.h"
#include <stdlib.h>
*/
import "C"

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unsafe"
)

const (
	// ZT_VirtualNetworkStatus OK (ZeroTierOne.h).
	statusOK = 1

	snapRefreshInterval = 100 * time.Millisecond
	framePollInterval   = 2 * time.Millisecond
	frameChannelCap     = 256
)

// Engine implements FrameSender.
var _ FrameSender = (*Engine)(nil)

type Engine struct {
	cfg     Config
	homeDir string

	node    *C.flclashtier_zt_node
	udpConn *net.UDPConn
	rawFile *os.File

	stopCh chan struct{}
	wg     sync.WaitGroup
	once   sync.Once

	mu   sync.RWMutex
	snap Snapshot
	gen  uint64

	routes *RouteTable

	frames chan Frame
}

// StartEngine creates the node, binds+protects the UDP socket, starts the
// driver loops and joins the configured network.
//
// protect is the Android VpnService.protect wrapper (may be nil on Linux
// test hosts). homeDir is the app files dir used for identity persistence.
func StartEngine(cfg Config, protect func(int), homeDir string) (*Engine, error) {
	e := &Engine{
		cfg:     cfg,
		homeDir: homeDir,
		stopCh:  make(chan struct{}),
		routes:  NewRouteTable(),
		frames:  make(chan Frame, frameChannelCap),
	}

	if homeDir != "" {
		cpath := C.CString(filepath.Join(homeDir, IdentityFileName))
		C.flclashtier_zt_set_identity_path(cpath)
		C.free(unsafe.Pointer(cpath))
	}

	node := C.flclashtier_zt_node_new()
	if node == nil {
		return nil, errors.New("ZT_Node_new failed")
	}
	e.node = node
	Infof("[ZT] node created: address=%010x", uint64(C.flclashtier_zt_node_address()))

	// Physical UDP socket: try the configured/default port, fall back to an
	// ephemeral port if busy.
	port := cfg.Port
	if port == 0 {
		port = 9993
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: port})
	if err != nil {
		conn, err = net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
		if err != nil {
			e.cleanup()
			return nil, fmt.Errorf("ListenUDP: %w", err)
		}
	}
	e.udpConn = conn

	rawFD, err := conn.File()
	if err != nil {
		e.cleanup()
		return nil, fmt.Errorf("udpConn.File: %w", err)
	}
	e.rawFile = rawFD // keep the dup alive for the whole engine lifetime
	fd := int(rawFD.Fd())

	// Android: exclude the ZT socket from the VPN (otherwise ZT UDP would
	// loop back into the TUN → ZT → …). No-op on Linux test hosts.
	if protect != nil {
		protect(fd)
	}
	C.flclashtier_zt_set_socket_fd(C.int(fd))
	Infof("[ZT] UDP socket bound on %s fd=%d", conn.LocalAddr().String(), fd)

	nwid, err := ParseNWID(cfg.NetworkID)
	if err != nil {
		e.cleanup()
		return nil, err
	}

	e.wg.Add(3)
	go e.receiveLoop()
	go e.backgroundLoop()
	go e.engineLoop()

	rc := C.flclashtier_zt_join(C.uint64_t(nwid))
	if rc != 0 {
		e.Stop()
		return nil, fmt.Errorf("join 0x%016x rc=%d", nwid, int(rc))
	}
	Infof("[ZT] join 0x%016x rc=%d", nwid, int(rc))
	return e, nil
}

// Stop tears the engine down in the M1-3 hardened order. Idempotent.
func (e *Engine) Stop() {
	e.once.Do(func() {
		close(e.stopCh)
		e.wg.Wait()
		e.cleanup()
		C.flclashtier_zt_set_socket_fd(C.int(-1))
		e.routes.Clear()
		e.mu.Lock()
		e.snap = Snapshot{}
		e.mu.Unlock()
	})
}

// cleanup closes the socket and deletes the node. Called from Stop() or from
// StartEngine error paths (loops not running yet — safe to call directly).
func (e *Engine) cleanup() {
	if e.udpConn != nil {
		e.udpConn.Close()
	}
	if e.rawFile != nil {
		e.rawFile.Close()
	}
	if e.node != nil {
		C.flclashtier_zt_node_delete(e.node)
		e.node = nil
	}
}

// ---- FrameSender implementation ----

// Current returns the latest config snapshot.
func (e *Engine) Current() (Snapshot, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.snap.Nwid == 0 || e.snap.Mac == 0 {
		return Snapshot{}, false
	}
	return e.snap, true
}

// MatchRoute returns the managed route covering addr.
func (e *Engine) MatchRoute(addr netip.Addr) *Route { return e.routes.Match(addr) }

// Ready reports whether at least one usable ZT managed route exists.
func (e *Engine) Ready() bool { return e.routes.Count() > 0 }

// Frames delivers Ethernet frames received from the ZT network.
func (e *Engine) Frames() <-chan Frame { return e.frames }

// SendFrame sends one Ethernet frame into the ZT network.
func (e *Engine) SendFrame(f Frame) error {
	if e.node == nil {
		return ErrNoNode
	}
	if len(f.Data) == 0 {
		return ErrEmptyFrame
	}
	var deadline int64
	rc := C.flclashtier_zt_send_frame(
		C.uint64_t(f.Nwid), C.uint64_t(f.SrcMAC), C.uint64_t(f.DstMAC),
		C.uint16_t(f.EtherType), C.uint16_t(f.VlanID),
		unsafe.Pointer(&f.Data[0]), C.uint(len(f.Data)),
		(*C.int64_t)(unsafe.Pointer(&deadline)),
	)
	if rc != 0 {
		return fmt.Errorf("ZT_Node_processVirtualNetworkFrame rc=%d", int(rc))
	}
	return nil
}

// SubscribeMulticast joins a multicast group on a network. adi is the
// ZeroTier multicast ADI (IPv4 ARP groups use the target IP as big-endian
// uint32; 0 otherwise).
func (e *Engine) SubscribeMulticast(nwid, mac uint64, adi uint32) error {
	if e.node == nil {
		return ErrNoNode
	}
	rc := C.flclashtier_zt_multicast_subscribe(C.uint64_t(nwid), C.uint64_t(mac), C.uint32_t(adi))
	if rc != 0 {
		return fmt.Errorf("multicastSubscribe rc=%d", int(rc))
	}
	return nil
}

// ---- driver loops ----

// receiveLoop feeds every received UDP datagram into the ZT core.
func (e *Engine) receiveLoop() {
	defer e.wg.Done()
	buf := make([]byte, 16384)
	for {
		select {
		case <-e.stopCh:
			return
		default:
		}
		_ = e.udpConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, from, err := e.udpConn.ReadFromUDP(buf)
		if err != nil {
			if isClosedErr(err) || isTimeoutErr(err) {
				continue
			}
			Warnf("[ZT] recv: %v", err)
			continue
		}
		sa := sockaddrFromUDPAddr(from)
		var deadline int64
		rc := C.flclashtier_zt_process_wire_packet(
			&sa,
			unsafe.Pointer(&buf[0]),
			C.uint(n),
			C.int64_t(time.Now().UnixMilli()),
			(*C.int64_t)(unsafe.Pointer(&deadline)),
		)
		if rc != 0 {
			Warnf("[ZT] processWirePacket rc=%d from %s", int(rc), from.String())
		}
	}
}

// backgroundLoop drives ZT timeouts/retries/path probes.
func (e *Engine) backgroundLoop() {
	defer e.wg.Done()
	next := int64(0)
	for {
		select {
		case <-e.stopCh:
			return
		default:
		}
		now := time.Now().UnixMilli()
		if next <= now {
			var newDeadline int64
			rc := C.flclashtier_zt_process_background_tasks(
				C.int64_t(now),
				(*C.int64_t)(unsafe.Pointer(&newDeadline)),
			)
			if rc != 0 {
				Warnf("[ZT] processBackgroundTasks rc=%d", int(rc))
			}
			next = newDeadline
		}
		sleepMs := next - time.Now().UnixMilli()
		if sleepMs < 10 {
			sleepMs = 10
		}
		if sleepMs > 1000 {
			sleepMs = 1000
		}
		select {
		case <-e.stopCh:
			return
		case <-time.After(time.Duration(sleepMs) * time.Millisecond):
		}
	}
}

// engineLoop pulls received frames (→ e.frames channel) and refreshes the
// config snapshot (routes/assigned addresses) on every generation change.
func (e *Engine) engineLoop() {
	defer e.wg.Done()
	lastSnap := time.Time{}
	lastRouteLog := ""
	for {
		select {
		case <-e.stopCh:
			return
		default:
		}
		var fr C.flclashtier_zt_frame
		if int(C.flclashtier_zt_frame_pull(&fr)) != 0 {
			data := C.GoBytes(unsafe.Pointer(&fr.data[0]), C.int(fr.len))
			f := Frame{
				Nwid:      uint64(fr.nwid),
				SrcMAC:    uint64(fr.srcMac),
				DstMAC:    uint64(fr.dstMac),
				EtherType: uint16(fr.etherType),
				VlanID:    uint16(fr.vlanId),
				Data:      data,
			}
			select {
			case e.frames <- f:
			default:
				// consumer backlog — drop newest, keep the flow moving
			}
			continue
		}
		if time.Since(lastSnap) >= snapRefreshInterval {
			if rl := e.refreshSnapshot(); rl != "" && rl != lastRouteLog {
				Infof("%s", rl)
				lastRouteLog = rl
			}
			lastSnap = time.Now()
		}
		select {
		case <-e.stopCh:
			return
		case <-time.After(framePollInterval):
		}
	}
}

// refreshSnapshot exports the C snapshot when the generation changed and
// rebuilds the route table. Returns a one-line route summary ("" = unchanged).
func (e *Engine) refreshSnapshot() string {
	var cs C.flclashtier_zt_snapshot
	gen := uint64(C.flclashtier_zt_snapshot_export(&cs))

	e.mu.RLock()
	curGen := e.gen
	e.mu.RUnlock()
	if gen == curGen {
		return ""
	}

	snap := Snapshot{
		Nwid:   uint64(cs.nwid),
		Status: int(cs.status),
		Mac:    uint64(cs.mac),
		MTU:    int(cs.mtu),
	}
	for i := 0; i < int(cs.assignedCount); i++ {
		a := cs.assigned[i]
		if a.family == 4 {
			var b4 [4]byte
			for j := 0; j < 4; j++ {
				b4[j] = byte(a.addr[j])
			}
			snap.Assigned = append(snap.Assigned, AssignedAddr{Addr: netip.AddrFrom4(b4), Bits: int(a.prefixLen)})
		} else if a.family == 6 {
			var b6 [16]byte
			for j := 0; j < 16; j++ {
				b6[j] = byte(a.addr[j])
			}
			snap.Assigned = append(snap.Assigned, AssignedAddr{Addr: netip.AddrFrom16(b6), Bits: int(a.prefixLen)})
		}
	}
	for i := 0; i < int(cs.routeCount); i++ {
		r := cs.routes[i]
		var pfx netip.Prefix
		switch r.family {
		case 4:
			var b4 [4]byte
			for j := 0; j < 4; j++ {
				b4[j] = byte(r.target[j])
			}
			pfx = netip.PrefixFrom(netip.AddrFrom4(b4), int(r.prefixLen))
		case 6:
			var b6 [16]byte
			for j := 0; j < 16; j++ {
				b6[j] = byte(r.target[j])
			}
			pfx = netip.PrefixFrom(netip.AddrFrom16(b6), int(r.prefixLen))
		default:
			continue
		}
		var via netip.Addr
		switch r.family {
		case 4:
			var vb [4]byte
			for j := 0; j < 4; j++ {
				vb[j] = byte(r.via[j])
			}
			if !allZero(vb[:]) {
				via = netip.AddrFrom4(vb)
			}
		case 6:
			var vb [16]byte
			for j := 0; j < 16; j++ {
				vb[j] = byte(r.via[j])
			}
			if !allZero(vb[:]) {
				via = netip.AddrFrom16(vb)
			}
		}
		snap.Routes = append(snap.Routes, Route{Prefix: pfx, Via: via, Flags: uint16(r.flags), Metric: uint16(r.metric)})
	}

	e.mu.Lock()
	e.snap = snap
	e.gen = gen
	e.mu.Unlock()

	if snap.Status == statusOK && snap.Nwid != 0 {
		e.routes.Set(snap.Routes)
	} else {
		e.routes.Clear()
	}
	return fmt.Sprintf("[ZT] config op=%d status=%d rev=%d mac=%012x assigned=%d routes=%d",
		int(cs.operation), snap.Status, uint64(cs.netconfRevision), snap.Mac, len(snap.Assigned), len(snap.Routes))
}

func allZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

// ---- helpers (stdlib only; no x/sys) ----

func isClosedErr(err error) bool {
	return errors.Is(err, os.ErrClosed) || errors.Is(err, net.ErrClosed)
}

func isTimeoutErr(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// sockaddrFromUDPAddr builds a C sockaddr_storage from a *net.UDPAddr.
// sin_port is already network byte order — write high byte first, NEVER htons
// (M1-3 root cause: double byte-swap corrupted peer ports).
func sockaddrFromUDPAddr(addr *net.UDPAddr) C.struct_sockaddr_storage {
	var sa C.struct_sockaddr_storage
	b := unsafe.Slice((*byte)(unsafe.Pointer(&sa)), 128)
	for i := range b {
		b[i] = 0
	}
	if v4 := addr.IP.To4(); v4 != nil {
		b[0] = 2 // AF_INET
		port := uint16(addr.Port)
		b[2] = byte(port >> 8)
		b[3] = byte(port & 0xff)
		copy(b[4:8], v4)
	} else {
		v6 := addr.IP.To16()
		b[0] = 10 // AF_INET6
		port := uint16(addr.Port)
		b[2] = byte(port >> 8)
		b[3] = byte(port & 0xff)
		copy(b[8:24], v6)
	}
	return sa
}
