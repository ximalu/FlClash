//go:build cgo

// Engine: Go-side ZeroTier node driver. Owns the node handle, the protected
// physical UDP socket, and the three driver loops:
//
//	receiveLoop   — UDP recv → processWirePacket (wire in)
//	backgroundLoop — deadline-driven processBackgroundTasks (timeouts/retries)
//	engineLoop     — frame queue pull + config snapshot refresh
//
// P0 lifecycle (2026-08-19): strict state machine + single active Engine.
// The C wrapper keeps a GLOBAL singleton ZeroTier Node (flclashtier_zt_node_new
// returns the same pointer while one exists) and a global socket fd — so the
// Go side mirrors that: only one Engine may exist at a time. StartEngine is
// idempotent for RUNNING, waits for STARTING/STOPPING to finish, and never
// creates a second Node/socket.
//
//	STOPPED → STARTING → RUNNING → STOPPING → STOPPED
//
// Stop order (learned from ZerotierFix + libzt comparison):
// close(stopCh) → close UDP socket (receiveLoop unblocks immediately) →
// wg.Wait() (no goroutine inside the C core) → node_delete → clear snapshot.
// The old 500ms-deadline polling is a timeout fallback, NOT the primary stop
// mechanism anymore.
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

	// bindRetries is how many times we retry the configured UDP port before
	// giving up. We NEVER fall back to a random port: changing the endpoint
	// silently breaks every peer's learned path (observed on 2026-08-19:
	// i3 saw the phone node as RELAY -1 after 9993 bind failed and the engine
	// grabbed a random port). Retrying gives a closing socket time to
	// actually release.
	bindRetries   = 5
	bindRetryWait = 200 * time.Millisecond
)

// EngineState is the public lifecycle state of an Engine.
type EngineState int

const (
	StateStopped EngineState = iota
	StateStarting
	StateRunning
	StateStopping
)

func (s EngineState) String() string {
	switch s {
	case StateStopped:
		return "STOPPED"
	case StateStarting:
		return "STARTING"
	case StateRunning:
		return "RUNNING"
	case StateStopping:
		return "STOPPING"
	}
	return "UNKNOWN"
}

// Engine implements FrameSender.
var _ FrameSender = (*Engine)(nil)

// Engine owns the ZeroTier node, the physical UDP socket and all driver
// goroutines. Exactly one Engine is active at a time (see globalEngineMu).
type Engine struct {
	cfg     Config
	homeDir string

	mu    sync.Mutex // guards state / node / conn
	state EngineState

	node    *C.flclashtier_zt_node
	udpConn *net.UDPConn

	stopCh chan struct{}
	wg     sync.WaitGroup

	snapMu sync.RWMutex
	snap   Snapshot
	gen    uint64

	routes *RouteTable

	frames chan Frame

	// bgDeadline is the shared next-background-task deadline (ms epoch).
	// receiveLoop updates it from processWirePacket's returned deadline;
	// backgroundLoop consumes it and only calls processBackgroundTasks when
	// the deadline has passed. This mirrors the official Android client
	// (ZeroTierOneService.run()) where one deadline variable is shared
	// between the receive and background paths — without this, a task that
	// processWirePacket schedules (path probe / NAT keepalive / retry) is
	// never observed by backgroundLoop until its own next tick, which can
	// be far away, letting peer paths age out while idle.
	bgMu       sync.Mutex
	bgDeadline int64
}

// ---- global singleton (mirrors the C wrapper's global s_node) ----

var (
	globalEngineMu sync.Mutex
	globalEngine   *Engine // non-nil while an Engine exists (any state)
	globalDone     chan struct{}
)

// currentEngine returns the active Engine under the global lock.
func currentEngine() *Engine {
	globalEngineMu.Lock()
	defer globalEngineMu.Unlock()
	return globalEngine
}

// setGlobalEngine installs e as the active engine and creates a fresh done
// channel. Caller must hold globalEngineMu.
func setGlobalEngine(e *Engine) {
	globalEngine = e
	globalDone = make(chan struct{})
}

// clearGlobalEngine removes e from the active slot and closes its done
// channel so waiters can proceed. Caller must hold globalEngineMu.
func clearGlobalEngine(e *Engine) {
	if globalEngine == e {
		globalEngine = nil
		if globalDone != nil {
			close(globalDone)
			globalDone = nil
		}
	}
}

// waitGlobalEngine waits until no Engine occupies the active slot.
// Safe to call when no engine exists (returns immediately).
func waitGlobalEngine() {
	globalEngineMu.Lock()
	done := globalDone
	eng := globalEngine
	globalEngineMu.Unlock()
	if eng == nil {
		return
	}
	if done != nil {
		<-done
	}
}

// WaitEngineStopped blocks until no Engine occupies the active slot, or the
// timeout elapses. Returns true if the slot is free, false on timeout.
//
// P0-3: FlClash's TUN teardown is asynchronous — handleStopTun closes the
// sing_tun listener, which makes the flowRouter's mihomoLoop exit and then
// calls eng.Stop() from that goroutine. A new StartEngine racing ahead of
// that teardown would see the old engine still RUNNING and idempotently
// return it, then the old shutdown would Stop the engine under the new
// flowRouter. Call this before StartEngine after a stop to serialize.
func WaitEngineStopped(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		globalEngineMu.Lock()
		eng := globalEngine
		done := globalDone
		globalEngineMu.Unlock()
		if eng == nil {
			return true
		}
		if done == nil {
			return false
		}
		select {
		case <-done:
			continue
		case <-time.After(time.Until(deadline)):
			return false
		}
	}
}

// StartEngine creates the node, binds+protects the UDP socket, starts the
// driver loops and joins the configured network.
//
// protect is the Android VpnService.protect wrapper (may be nil on Linux
// test hosts). homeDir is the app files dir used for identity persistence.
//
// Lifecycle guarantees:
//   - If an Engine is RUNNING, StartEngine returns it (idempotent, no new
//     Node/socket/goroutines).
//   - If an Engine is STARTING, StartEngine waits for it and returns it.
//   - If an Engine is STOPPING, StartEngine waits for full STOPPED then
//     creates a fresh Engine.
//   - Binding the configured port is mandatory: a bind failure is retried
//     bindRetries times, then StartEngine FAILS. It never silently falls
//     back to a random port.
func StartEngine(cfg Config, protect func(int), homeDir string) (*Engine, error) {
	// Fast path: an engine is already running — return it as-is.
	globalEngineMu.Lock()
	if globalEngine != nil {
		switch globalEngine.getState() {
		case StateRunning:
			e := globalEngine
			globalEngineMu.Unlock()
			return e, nil
		case StateStarting:
			// Concurrent start in progress: wait for it to finish, then
			// return the winner (it will be RUNNING by then or we failed).
			done := globalDone
			globalEngineMu.Unlock()
			if done != nil {
				<-done
			}
			return StartEngine(cfg, protect, homeDir)
		case StateStopping:
			// A stop is in flight: wait for full teardown, then build fresh.
			done := globalDone
			globalEngineMu.Unlock()
			if done != nil {
				<-done
			}
			return StartEngine(cfg, protect, homeDir)
		}
	}
	// No active engine (or stale STOPPED slot): build a new one.
	e := &Engine{
		cfg:     cfg,
		homeDir: homeDir,
		state:   StateStarting,
		stopCh:  make(chan struct{}),
		routes:  NewRouteTable(),
		frames:  make(chan Frame, frameChannelCap),
	}
	setGlobalEngine(e)
	globalEngineMu.Unlock()

	if err := e.start(cfg, protect, homeDir); err != nil {
		e.abort(err)
		return nil, err
	}
	e.setState(StateRunning)
	Infof("[ZT] engine RUNNING (node=%010x port=%d)", e.nodeAddress(), e.boundPort())
	return e, nil
}

// start performs the actual bring-up. On any error the caller must call
// abort() to release the global slot.
func (e *Engine) start(cfg Config, protect func(int), homeDir string) error {
	if homeDir != "" {
		cpath := C.CString(filepath.Join(homeDir, IdentityFileName))
		C.flclashtier_zt_set_identity_path(cpath)
		C.free(unsafe.Pointer(cpath))
	}

	node := C.flclashtier_zt_node_new()
	if node == nil {
		return errors.New("ZT_Node_new failed")
	}
	e.mu.Lock()
	e.node = node
	e.mu.Unlock()
	Infof("[ZT] node created: address=%010x", uint64(C.flclashtier_zt_node_address()))

	// Physical UDP socket: bind the configured/default port. NEVER a random
	// fallback — endpoint stability is a hard invariant (P0-1).
	port := cfg.Port
	if port == 0 {
		port = DefaultPort
	}
	conn, err := bindPort(port)
	if err != nil {
		return fmt.Errorf("bind UDP port %d: %w", port, err)
	}
	e.mu.Lock()
	e.udpConn = conn
	e.mu.Unlock()

	// Get the raw fd WITHOUT detaching the socket from Go's runtime poller.
	// ⚠️ P0-3 deadlock root cause (2026-08-19): the previous code used
	// conn.File(), which dup()s the fd AND detaches the original from the
	// poller — after that, ReadFromUDP ignores SetReadDeadline and blocks
	// forever in a raw syscall. Meanwhile the C wrapper held the dup fd, so
	// conn.Close() could not actually close the socket, so FD.Close() waited
	// on the stuck read's readLock → Stop() deadlocked (seen in TestD 100x
	// stress). SyscallConn() hands out the original fd without dup and
	// without poller detachment: SetReadDeadline keeps working and Close()
	// unblocks a pending read (verified experimentally).
	fd, err := socketFd(conn)
	if err != nil {
		return fmt.Errorf("udpConn.SyscallConn: %w", err)
	}

	// Android: exclude the ZT socket from the VPN (otherwise ZT UDP would
	// loop back into the TUN → ZT → …). No-op on Linux test hosts.
	if protect != nil {
		protect(fd)
	}
	C.flclashtier_zt_set_socket_fd(C.int(fd))
	Infof("[ZT] UDP socket bound on %s fd=%d", conn.LocalAddr().String(), fd)

	nwid, err := ParseNWID(cfg.NetworkID)
	if err != nil {
		return err
	}

	e.wg.Add(3)
	go e.receiveLoop()
	go e.backgroundLoop()
	go e.engineLoop()

	rc := C.flclashtier_zt_join(C.uint64_t(nwid))
	if rc != 0 {
		return fmt.Errorf("join 0x%016x rc=%d", nwid, int(rc))
	}
	Infof("[ZT] join 0x%016x rc=%d", nwid, int(rc))
	return nil
}

// socketFd returns the underlying fd of a UDPConn without dup and without
// detaching it from the runtime poller (see start() for why this matters).
func socketFd(conn *net.UDPConn) (int, error) {
	rc, err := conn.SyscallConn()
	if err != nil {
		return -1, err
	}
	fd := -1
	var ctlErr error
	if err := rc.Control(func(f uintptr) {
		fd = int(f)
	}); err != nil {
		return -1, err
	}
	if ctlErr != nil {
		return -1, ctlErr
	}
	if fd < 0 {
		return -1, errors.New("SyscallConn.Control returned no fd")
	}
	return fd, nil
}

// abort cleans up a failed start and releases the global slot. Idempotent.
// Must mirror Stop's ordering: stop accepting work → close socket (unblocks
// receiveLoop) → wg.Wait() → node_delete — because goroutines may already be
// running when a late start step (e.g. join) fails.
func (e *Engine) abort(startErr error) {
	Warnf("[ZT] engine start failed: %v", startErr)
	select {
	case <-e.stopCh:
		// already closed
	default:
		close(e.stopCh)
	}
	e.mu.Lock()
	if e.udpConn != nil {
		e.udpConn.Close()
		e.udpConn = nil
	}
	e.mu.Unlock()
	e.wg.Wait()
	e.mu.Lock()
	if e.node != nil {
		C.flclashtier_zt_node_delete(e.node)
		e.node = nil
	}
	e.state = StateStopped
	e.mu.Unlock()
	C.flclashtier_zt_set_socket_fd(C.int(-1))

	globalEngineMu.Lock()
	clearGlobalEngine(e)
	globalEngineMu.Unlock()
}

// Stop tears the engine down in the hardened order. Idempotent.
//
//  1. close(stopCh)            — stop accepting new work
//  2. close UDP socket          — receiveLoop unblocks immediately
//  3. wg.Wait()                 — no goroutine inside the C core anymore
//  4. node_delete               — safe to release the C node
//  5. clear snapshot / routes
//  6. release the global slot   — waiters can build a fresh Engine
func (e *Engine) Stop() {
	e.mu.Lock()
	if e.state == StateStopped {
		e.mu.Unlock()
		return
	}
	e.state = StateStopping
	e.mu.Unlock()

	close(e.stopCh)

	// Closing the socket makes receiveLoop's ReadFromUDP return immediately
	// (use of closed network connection) — it no longer waits up to 500ms.
	e.mu.Lock()
	if e.udpConn != nil {
		e.udpConn.Close()
		e.udpConn = nil
	}
	e.mu.Unlock()

	e.wg.Wait()

	e.mu.Lock()
	if e.node != nil {
		C.flclashtier_zt_node_delete(e.node)
		e.node = nil
	}
	e.state = StateStopped
	e.mu.Unlock()
	C.flclashtier_zt_set_socket_fd(C.int(-1))

	e.routes.Clear()
	e.snapMu.Lock()
	e.snap = Snapshot{}
	e.gen = 0
	e.snapMu.Unlock()

	globalEngineMu.Lock()
	clearGlobalEngine(e)
	globalEngineMu.Unlock()
	Infof("[ZT] engine STOPPED")
}

func (e *Engine) getState() EngineState {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state
}

func (e *Engine) setState(s EngineState) {
	e.mu.Lock()
	e.state = s
	e.mu.Unlock()
}

// nodeAddress returns the ZT node address (0 if node is gone).
func (e *Engine) nodeAddress() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.node == nil {
		return 0
	}
	return uint64(C.flclashtier_zt_node_address())
}

// boundPort returns the local UDP port the engine is bound to (0 if none).
func (e *Engine) boundPort() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.udpConn == nil {
		return 0
	}
	if la, ok := e.udpConn.LocalAddr().(*net.UDPAddr); ok {
		return la.Port
	}
	return 0
}

// ---- FrameSender implementation ----

// Current returns the latest config snapshot.
func (e *Engine) Current() (Snapshot, bool) {
	e.snapMu.RLock()
	defer e.snapMu.RUnlock()
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
	e.mu.Lock()
	node := e.node
	e.mu.Unlock()
	if node == nil {
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
	e.mu.Lock()
	node := e.node
	e.mu.Unlock()
	if node == nil {
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
// Primary exit: socket close (ReadFromUDP returns use-of-closed-network).
// The 500ms read deadline is a fallback so a wedged read still lets the
// loop observe stopCh — it is NOT the stop mechanism anymore.
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
			if isClosedErr(err) {
				// Socket closed by Stop(): exit immediately. This is the
				// primary stop path — do NOT continue polling.
				return
			}
			if isTimeoutErr(err) {
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
		// Share the returned deadline with backgroundLoop (official client
		// semantics): if the core scheduled something soon, wake the
		// background task runner instead of sleeping until its own tick.
		if deadline > 0 {
			e.bgMu.Lock()
			if e.bgDeadline == 0 || deadline < e.bgDeadline {
				e.bgDeadline = deadline
			}
			e.bgMu.Unlock()
		}
	}
}

// backgroundLoop drives ZT timeouts/retries/path probes.
// It consumes the shared bgDeadline (updated by receiveLoop from
// processWirePacket) so tasks scheduled by inbound packets are honored
// promptly — critical for NAT keepalive and path probing while idle.
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
		// Pull in any earlier deadline published by receiveLoop.
		e.bgMu.Lock()
		if e.bgDeadline > 0 && (next == 0 || e.bgDeadline < next) {
			next = e.bgDeadline
		}
		e.bgMu.Unlock()
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
			e.bgMu.Lock()
			e.bgDeadline = 0 // consumed
			e.bgMu.Unlock()
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

	e.snapMu.RLock()
	curGen := e.gen
	e.snapMu.RUnlock()
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

	e.snapMu.Lock()
	e.snap = snap
	e.gen = gen
	e.snapMu.Unlock()

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
