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
	"strings"
	"sync"
	"sync/atomic"
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

	node *C.flclashtier_zt_node

	// udpConn 是物理 UDP socket。用 atomic.Pointer 而非普通字段：RebuildSocket
	// 需要在不停止 receiveLoop 的前提下热替换 socket（2026-11-17 现场：
	// WiFi→蜂窝后旧 NAT 映射过期，ZT 永远打不了洞，只能重启 App）。
	// receiveLoop 每轮 .Load() 取当前值，重建时不会退出 goroutine。
	udpConn atomic.Pointer[net.UDPConn]
	// udpConn6 是可选的 IPv6 物理 socket（见 bind.go listenUDPReuse6）。
	// nil = 本机没有可用 IPv6，或该端口 v6 绑定失败（不致命）。
	udpConn6 atomic.Pointer[net.UDPConn]

	// protectFn 记住 StartEngine 传入的 protect 回调（返回值 = 该 fd 是否真的
	// 被排除在 VPN 之外，并已绑到当前物理网络）。RebuildSocket 必须对新 fd
	// 重新 protect，否则新 socket 会重新被 TUN 捕获；返回 false 时必须
	// fail closed —— 带着一个仍会被自己 TUN 捕获的 socket 继续跑等于让 ZT
	// UDP 进 TUN 形成环路，故障现象比「启动失败」难查得多。
	protectFn func(int) bool

	// socketGen：物理 UDP socket 世代号，与 TUN 的 gen 完全独立
	// （socketGeneration / tunGeneration 分离，2026-11-17 review 结论）。
	socketGen atomic.Uint64

	// rebuilding 防止 watchdog 与手动触发并发重建。
	rebuilding atomic.Bool

	// restarting 防止并发重建「整个 ZT Node」（比换 socket 更重的一档）。
	restarting atomic.Bool

	// lifecycleMu 串行化 Stop 与 RestartNode：两者都在拆 node / socket，
	// 交错执行会让 node_delete 与 node_new 撞在一起（C wrapper 的 node 是
	// 全局单例，交错 = use-after-free）。
	lifecycleMu sync.Mutex

	// nodeGen：C 侧 ZT_Node 被创建过几次（1 = 首次启动，2+ = 中途重建过）。
	// 与 socketGen 分开看：2026-11-19 实机日志里 socketGen 涨到 20 而
	// nodeGen 一直是 1，正是「只换 socket、从不重建 node」的证据。
	nodeGen atomic.Uint64

	// socketWatch 状态：判定“有出站、零入站”的通道僵死。
	wdMu        sync.Mutex
	wdLastRxAt  time.Time
	wdLastRxPkt uint64
	wdTxAtRx    uint64
	wdLastFix   time.Time
	// wdFixes 是自愈**尝试**次数（在 RebuildSocket 之前就递增，历史命名），
	// wdFixOK 是 RebuildSocket 返回 nil 的**成功**次数（只代表 socket 换成了，
	// 不代表链路恢复——后者看 wdRecovery*）。
	wdFixes      int
	wdFixOK      int
	wdHealth     string    // 上次采样判定：ok|idle|stalled|rebuilding（唯一判据实现点）
	wdLastSample time.Time // 上次采样时刻，用于识别采样循环被挂起（Doze）

	// 数据面僵死观测（2026-11-19 实机）：换网后物理层仍能收到对端/根服务器的
	// keepalive（wire rx 每 10s 还在涨），但业务帧零进展（帧不进 flow router、
	// TUN 写不出去）。「有出站、零入站」判据永远看不到这种僵死，所以单独记
	// 一条：帧最近什么时候前进过。
	ztFramesRx      atomic.Uint64 // 交付给 flow router 的 ZT 帧数 = datapath 日志的 frames rx
	ztFramesTx      atomic.Uint64 // 送进 overlay 的业务帧数（SendFrame 成功）= 出站需求的真实来源
	wdFrameAt       time.Time     // 数据帧最近一次前进的时刻（Doze 复位时重设）
	wdFrameMark     uint64        // 上次前进时的 ztFramesRx
	wdFrameTxAt     uint64        // 上次前进时的 sendDemand
	wdFrameFixAt    time.Time     // 上次为数据面僵死重建 socket 的时刻
	wdFrameStalls   int           // 判定数据面僵死的次数
	wdFrameRebuilds int           // 为数据面僵死重建 socket 的次数
	wdFrameFrozenMs int64         // 最近一次判定的冻结时长（暴露给上层，不因重建而清零）
	wdNodeRestarts  int           // 为数据面僵死重建整个 ZT Node 的次数
	wdFrameNodeAt   time.Time     // 上次重建 Node 的时刻（升级限速用）

	// 重建后的链路恢复确认：重建 ≠ 恢复。记录重建时的 socketGen / 入站计数，
	// 观察窗口内出现新的 UDP 入站才算 confirmed，超时则记 timeout。
	wdRecoveryGen    uint64
	wdRecoveryRxBase uint64
	wdRecoveryUntil  time.Time
	wdRecoveryDone   bool
	// 恢复确认四态（与网络页一致）：
	//   none        没有重建过，谈不上恢复
	//   pending     socket 已重建，正在等证据
	//   recovered   观察窗口内出现新的 UDP 入站 → 物理接收恢复（唯一正面证据）
	//   inconclusive 观察窗口结束仍无入站 → **不等于失败**，只是没机会验证
	// failed 目前**永不**产生：ZT 层没有可用的主动验证手段，出站包也
	// 证明不了对端收到；业务层 TCP 探测失败属于另一个层面，不并入这里。
	wdRecoveryState        string // none|pending|recovered|inconclusive
	wdRecoveryOK           int
	wdRecoveryInconclusive int

	stopCh chan struct{}
	wg     sync.WaitGroup

	snapMu sync.RWMutex
	snap   Snapshot
	gen    uint64

	routes     *RouteTable
	// lastRoutes 是**最近一次有效**的 managed routes。teardown / node 重建 /
	// 换网掉线期间 e.routes 会清空（Ready()=false），但「这个目的地属于 ZT
	// 网段」这个事实没变。flow router 用它把 ZT 流量挡在 mihomo 之外。
	lastRoutes *RouteTable

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

	// framesDropped counts frames dropped because the Go frames channel was
	// full (the consumer — adapter → TUN write — could not keep up).
	framesDropped atomic.Uint64

	// 分层计数：物理 UDP 收到多少（wireRx*）、其中 core 处理成功/失败多少。
	// 与 framesDropped / adapter 计数器合起来就能定位「包死在哪一层」。
	wireRxPkts       atomic.Uint64
	wireRx6Pkts      atomic.Uint64
	netNodeRestartAt atomic.Int64
	wireRxBytes      atomic.Uint64
	coreProcOK       atomic.Uint64
	coreProcErr      atomic.Uint64
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
func StartEngine(cfg Config, protect func(int) bool, homeDir string) (*Engine, error) {
	// Fast path: an engine is already running — return it as-is.
	globalEngineMu.Lock()
	if globalEngine != nil {
		switch globalEngine.getState() {
		case StateRunning:
			e := globalEngine
			globalEngineMu.Unlock()
			// 2026-11-24 现场：VpnService 重启后是**新实例**，旧 protect 回调
			// 已经失效（protect(fd) 恒 false）。引擎是长命的，回调必须跟着换，
			// 否则用户每关开一次 VPN，之后所有 socket/节点重建都会 fail closed
			// —— 日志表现就是 "protect(fd=...) failed: socket would loop back
			// into our own TUN"，引擎变成僵尸，只能杀进程。
			e.setProtectFn(protect)
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
		routes:     NewRouteTable(),
		lastRoutes: NewRouteTable(),
		frames:  make(chan Frame, frameChannelCap),
	}
	setGlobalEngine(e)
	globalEngineMu.Unlock()

	if err := e.start(cfg, protect, homeDir); err != nil {
		recordStartError(err)
		e.abort(err)
		return nil, err
	}
	clearStartError()
	e.setState(StateRunning)
	Infof("[ZT] engine RUNNING (node=%010x port=%d)", e.nodeAddress(), e.boundPort())
	return e, nil
}

// start performs the actual bring-up. On any error the caller must call
// abort() to release the global slot.
func (e *Engine) start(cfg Config, protect func(int) bool, homeDir string) error {
	diagSetHome(homeDir)
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
	nodeGen := e.nodeGen.Add(1)
	Infof("[ZT] node created: address=%010x nodeGen=%d", uint64(C.flclashtier_zt_node_address()), nodeGen)

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
	e.udpConn.Store(conn)
	e.mu.Lock()
	e.protectFn = protect
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

	// Android: protect() 只保证 socket 绕开我们自己的 TUN；Kotlin 侧在同一次
	// 回调里还会把 fd 绑到当前物理网络（Network.bindSocket），这才是新 socket
	// 能跟着 WiFi↔蜂窝切换走的关键。返回 false = 这个 fd 仍会被自己的 TUN
	// 捕获 → fail closed，宁可启动失败也不要一条静默坏掉的数据面。
	// Linux 测试主机 protect == nil，整段跳过。
	if protect != nil && !protect(fd) {
		_ = conn.Close()
		return fmt.Errorf("protect(fd=%d) failed: socket would loop back into our own TUN", fd)
	}
	C.flclashtier_zt_set_socket_fd(C.int(fd))
	gen := e.socketGen.Add(1)
	Infof("[ZT-SOCKET] bind gen=%d addr=%s fd=%d", gen, conn.LocalAddr().String(), fd)
	e.attachSocket6(port, protect)
	e.DiagSnapshot("socket-bind")

	nwid, err := ParseNWID(cfg.NetworkID)
	if err != nil {
		return err
	}

	e.wg.Add(5)
	go e.receiveLoop()
	go e.receiveLoop6()
	go e.backgroundLoop()
	go e.engineLoop()
	go e.socketWatchdog()

	rc := C.flclashtier_zt_join(C.uint64_t(nwid))
	if rc != 0 {
		return fmt.Errorf("join 0x%016x rc=%d", nwid, int(rc))
	}
	Infof("[ZT] join 0x%016x rc=%d", nwid, int(rc))
	e.DiagSnapshot("engine-start")
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
	if c := e.udpConn.Swap(nil); c != nil {
		c.Close()
	}
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

// Stop tears the engine down and releases the global slot. Idempotent.
//
// Stop = teardown()（拆除 node/socket/goroutine，Engine 对象保留）+ 释放全局
// 槽位。拆除顺序单独拆成 teardown 是为了让 RestartNode 复用同一条安全路径：
// 「关 VPN」和「换网僵死后重建 node」必须用同一套顺序，否则两条路径迟早
// 各自演化出不同的竞态。
func (e *Engine) Stop() {
	e.lifecycleMu.Lock()
	defer e.lifecycleMu.Unlock()

	wasRunning := e.getState() != StateStopped
	e.teardown()

	globalEngineMu.Lock()
	clearGlobalEngine(e)
	globalEngineMu.Unlock()
	if wasRunning {
		// 关闭 VPN = 一次完整的运行周期结束，启动错误随之失效。
		clearStartError()
		Infof("[ZT] engine STOPPED")
	}
}

// teardown 按加固顺序拆除驱动循环、物理 socket 与 C node，但**保留 Engine
// 对象本身**（frames channel / routes / cfg / protectFn 都留着），这样重建
// 之后 flow router 与 adapter 持有的 *Engine 依旧有效。
//
//  1. close(stopCh)            — stop accepting new work
//  2. close UDP socket          — receiveLoop unblocks immediately
//  3. wg.Wait()                 — no goroutine inside the C core anymore
//  4. node_delete               — safe to release the C node
//  5. clear snapshot / routes / bg deadline
//  6. 换一条新的 stopCh          — 留给下一次 start 用的干净通道
//
// 调用方必须持有 lifecycleMu。
func (e *Engine) teardown() {
	e.mu.Lock()
	if e.state == StateStopped {
		e.mu.Unlock()
		return
	}
	e.state = StateStopping
	stopCh := e.stopCh
	e.mu.Unlock()

	Infof("[ZT-TEARDOWN] 1/6 stopCh closed (state=STOPPING)")
	close(stopCh)

	// Closing the socket makes receiveLoop's ReadFromUDP return immediately
	// (use of closed network connection) — it no longer waits up to 500ms.
	if c := e.udpConn.Swap(nil); c != nil {
		_ = c.Close()
	}
	if c := e.udpConn6.Swap(nil); c != nil {
		_ = c.Close()
	}
	C.flclashtier_zt_set_socket_fd6(C.int(-1))
	Infof("[ZT-TEARDOWN] 2/6 UDP socket closed")

	// 带超时的等待：正常应在毫秒级退出。超时 = 某个 goroutine 卡在 C core
	// 里，或（2026-11-24 实机）在 wg 内的 goroutine 里同步调 RestartNode →
	// teardown 等自己。这时不能无限等，必须把 goroutine 栈落盘，否则下次
	// 还是只有一行 "restarting ZeroTier node" 看不出死在哪。
	waitDone := make(chan struct{})
	go func() {
		e.wg.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
		Infof("[ZT-TEARDOWN] 3/6 goroutines exited")
	case <-time.After(3 * time.Second):
		Warnf("[ZT-TEARDOWN] goroutines still inside the core after 3s — dumping stacks (self-deadlock?)")
		// 只 dump 栈：此时可能有 goroutine 正持有 C 侧 node mutex，
		// 再去取 peer 快照会把诊断自己也卡死。
		diagDumpGoroutines("teardown-stuck")
		<-waitDone
		Infof("[ZT-TEARDOWN] 3/6 goroutines exited (late)")
	}
	if e.node != nil {
		Infof("[ZT-TEARDOWN] 4/6 releasing ZeroTier node")
	}

	e.mu.Lock()
	if e.node != nil {
		C.flclashtier_zt_node_delete(e.node)
		e.node = nil
	}
	e.state = StateStopped
	e.stopCh = make(chan struct{})
	e.mu.Unlock()
	C.flclashtier_zt_set_socket_fd(C.int(-1))

	e.routes.Clear()
	e.snapMu.Lock()
	e.snap = Snapshot{}
	e.gen = 0
	e.snapMu.Unlock()

	e.bgMu.Lock()
	e.bgDeadline = 0
	e.bgMu.Unlock()
	Infof("[ZT-TEARDOWN] 6/6 done")
	e.DiagSnapshot("teardown-done")
}

// RestartNode 重建整个 ZeroTier Node：走同一套 teardown，然后用**同一个
// Engine 对象**重新 node_new + bind + protect + join。
//
// 为什么需要它（2026-11-19 真机回归）：RebuildSocket 只换物理 fd，换完之后
// frames 依旧是 0（gen=2 空转 39s / 80s），真正见效的动作只有「杀进程重启」。
// 整包重启（SetupAction.setRunning）代价太大，而且走的还是「重启 mihomo +
// flow router」那条路 —— 日志里连一条 `[ZT] node created` 都没有。这里在 Go
// 内部重建 node，Engine 指针、frames channel、flow router 全部不用动。
//
// 与 Stop 的区别：不释放全局槽位（同一个 Engine 继续占用），所以并发进来的
// StartEngine 仍然幂等拿到它，不会出现「新 flow router 挂在已停止引擎上」。
func (e *Engine) RestartNode(reason string) error {
	if !e.restarting.CompareAndSwap(false, true) {
		return errors.New("node restart already in progress")
	}
	defer e.restarting.Store(false)

	e.lifecycleMu.Lock()
	defer e.lifecycleMu.Unlock()

	if e.getState() == StateStopped {
		return errors.New("engine stopped")
	}
	cfg, protect, homeDir := e.cfg, e.protectFn, e.homeDir

	Warnf("[ZT] restarting ZeroTier node (reason %q): socket rebuild alone did not restore frames", reason)
	e.DiagSnapshot("node-restart-begin")

	e.teardown()
	if err := e.start(cfg, protect, homeDir); err != nil {
		recordStartError(err)
		e.abort(err) // 重建失败：释放全局槽位，下一次 TUN 启动会建全新 Engine
		e.DiagSnapshot("node-restart-failed")
		return err
	}
	e.setState(StateRunning)
	Infof("[ZT] node restarted (reason %q): node=%010x port=%d nodeGen=%d socketGen=%d",
		reason, e.nodeAddress(), e.boundPort(), e.nodeGen.Load(), e.socketGen.Load())
	e.DiagSnapshot("node-restart-ok")
	return nil
}

// setProtectFn 更新物理 socket 的 protect 回调（VpnService 实例换了就要换）。
// markNetworkNodeRestart 给「换网触发的 node 重建」限速：45 秒一次。
func (e *Engine) markNetworkNodeRestart() bool {
	now := time.Now()
	if !e.netNodeRestartAt.CompareAndSwap(0, now.UnixMilli()) {
		prev := e.netNodeRestartAt.Load()
		if now.UnixMilli()-prev < 45_000 {
			return false
		}
		e.netNodeRestartAt.Store(now.UnixMilli())
	}
	return true
}

func (e *Engine) setProtectFn(protect func(int) bool) {
	if protect == nil {
		return
	}
	e.mu.Lock()
	e.protectFn = protect
	e.mu.Unlock()
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
	conn := e.udpConn.Load()
	if conn == nil {
		return 0
	}
	if la, ok := conn.LocalAddr().(*net.UDPAddr); ok {
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

// MatchLastKnownRoute 判断 addr 是否落在**最近一次有效**的 ZT managed routes 里。
// 换网 / node 重建期间的 e.routes 是空的，但命中这里的包仍然属于 ZT 网段：
// 必须丢弃（等 ZT 恢复后由上层 TCP 重传），绝不能交给 mihomo——真机实测
// 2026-11-25 22:10:44 起 mihomo 把 192.168.196.81:4533 当 192.168.0.0/16
// Direct 直连，3.5 分钟全是 i/o timeout。
func (e *Engine) MatchLastKnownRoute(addr netip.Addr) *Route {
	if e == nil || e.lastRoutes == nil {
		return nil
	}
	return e.lastRoutes.Match(addr)
}

// logPeerPaths 打一行 peer/path 摘要（诊断用）：address role lat paths pref lastRx。
func (e *Engine) logPeerPaths(stage string) {
	snap := e.PeerSnapshot()
	if !snap.Known {
		Infof("[ZT-PEERS] %s: no live node", stage)
		return
	}
	now := time.Now().UnixMilli()
	parts := make([]string, 0, len(snap.Peers))
	for _, p := range snap.Peers {
		lastRx := int64(-1)
		if p.LastReceiveAtMs > 0 {
			lastRx = (now - p.LastReceiveAtMs) / 1000
		}
		parts = append(parts, fmt.Sprintf("%s[R%s lat=%d paths=%d pref=%d lastRx=%ds]",
			p.Address, p.Role, p.LatencyMs, p.PathCount, p.PreferredPaths, lastRx))
	}
	Infof("[ZT-PEERS] %s (%d peers): %s", stage, len(snap.Peers), strings.Join(parts, " "))
}

// Ready reports whether at least one usable ZT managed route exists.
func (e *Engine) Ready() bool { return e.routes.Count() > 0 }

// Frames delivers Ethernet frames received from the ZT network.
func (e *Engine) Frames() <-chan Frame { return e.frames }

// FramesDropped reports how many inbound frames were dropped because the
// frames channel was full (consumer backpressure).
func (e *Engine) FramesDropped() uint64 { return e.framesDropped.Load() }

// FrameQueueCapacity reports the C-side received-frame ring queue capacity.
func FrameQueueCapacity() int { return int(C.flclashtier_zt_frame_queue_capacity()) }

// DataPathCounters exposes the C-side ring-queue drop count and the physical
// wire send statistics (successes plus errno classes). These are the numbers
// that separate "packets lost inside our pipeline" from "packets lost on the
// physical ZeroTier path".
func (e *Engine) DataPathCounters() (frameDropped, sendOK, sendEAGAIN, sendENOBUFS, sendOther uint64) {
	var ok, ea, en, ot C.uint64_t
	C.flclashtier_zt_wire_send_stats(&ok, &ea, &en, &ot)
	return uint64(C.flclashtier_zt_frame_dropped()), uint64(ok), uint64(ea), uint64(en), uint64(ot)
}

// WireRecvCounters 返回物理侧收包与 core 处理的计数：
// rxPkts/rxBytes 是 UDP socket 真实收到的，procOK/procErr 是交给 core 后
// processWirePacket 的结果。三层（wire rx → core process → frame rx）
// 对不上就能判断包死在哪一层。
func (e *Engine) WireRecvCounters() (rxPkts, rxBytes, procOK, procErr uint64) {
	return e.wireRxPkts.Load(), e.wireRxBytes.Load(), e.coreProcOK.Load(), e.coreProcErr.Load()
}

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
	// processVirtualNetworkFrame also returns a next-background-task deadline
	// (path probing / retransmission scheduling). Dropping it deferred work the
	// core had already asked for, so outbound traffic could stall while
	// backgroundLoop kept sleeping on a far-away deadline.
	e.ztFramesTx.Add(1)
	e.publishDeadline(deadline)
	return nil
}

// publishDeadline records an earlier background-task deadline requested by a
// ZT_Node_* call. Every ZT entry point (processWirePacket,
// processVirtualNetworkFrame, processBackgroundTasks) returns one; the earliest
// wins so backgroundLoop never sleeps past work the core scheduled.
func (e *Engine) publishDeadline(d int64) {
	if d <= 0 {
		return
	}
	e.bgMu.Lock()
	if e.bgDeadline == 0 || d < e.bgDeadline {
		e.bgDeadline = d
	}
	e.bgMu.Unlock()
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
// attachSocket6 尝试为物理路径加一条 IPv6 socket。失败只记日志、不阻断启动：
// WiFi 只有 IPv4 时它本来就是可有可无的，蜂窝/公网才是它的价值所在。
func (e *Engine) attachSocket6(port int, protect func(int) bool) {
	conn, err := listenUDPReuse6(port)
	if err != nil {
		Infof("[ZT-SOCKET] ipv6 socket unavailable on port %d: %v", port, err)
		return
	}
	fd, err := socketFd(conn)
	if err != nil {
		_ = conn.Close()
		Warnf("[ZT-SOCKET] ipv6 SyscallConn: %v", err)
		return
	}
	if protect != nil && !protect(fd) {
		_ = conn.Close()
		Warnf("[ZT-SOCKET] protect(fd=%d) failed on ipv6 socket: keeping IPv4 only", fd)
		return
	}
	e.udpConn6.Store(conn)
	C.flclashtier_zt_set_socket_fd6(C.int(fd))
	Infof("[ZT-SOCKET] ipv6 socket bound addr=%s fd=%d", conn.LocalAddr().String(), fd)
}

func (e *Engine) receiveLoop() {
	e.receiveLoopOn(&e.udpConn, false)
}

func (e *Engine) receiveLoop6() {
	e.receiveLoopOn(&e.udpConn6, true)
}

func (e *Engine) receiveLoopOn(holder *atomic.Pointer[net.UDPConn], v6 bool) {
	defer e.wg.Done()
	buf := make([]byte, 16384)
	for {
		select {
		case <-e.stopCh:
			return
		default:
		}
		conn := holder.Load()
		if conn == nil {
			// RebuildSocket 正在热替换 socket：等它装好再继续，不要退出。
			select {
			case <-e.stopCh:
				return
			case <-time.After(20 * time.Millisecond):
			}
			continue
		}
		_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			if isClosedErr(err) {
				// Stop() 关 socket → 退出；RebuildSocket 关 socket → 换上
				// 新 conn 继续跑（不退出 goroutine）。
				select {
				case <-e.stopCh:
					return
				default:
				}
				if e.rebuilding.Load() {
					continue
				}
				return
			}
			if isTimeoutErr(err) {
				continue
			}
			Warnf("[ZT] recv: %v", err)
			continue
		}
		e.wireRxPkts.Add(1)
		e.wireRxBytes.Add(uint64(n))
		if v6 {
			e.wireRx6Pkts.Add(1)
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
			e.coreProcErr.Add(1)
			Warnf("[ZT] processWirePacket rc=%d from %s", int(rc), from.String())
		} else {
			e.coreProcOK.Add(1)
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
			// Consume any published deadline BEFORE entering the core. The old
			// order (call the core, then zero the field) lost a deadline that was
			// published while the core call was running.
			e.bgMu.Lock()
			e.bgDeadline = 0
			e.bgMu.Unlock()
			var newDeadline int64
			rc := C.flclashtier_zt_process_background_tasks(
				C.int64_t(now),
				(*C.int64_t)(unsafe.Pointer(&newDeadline)),
			)
			if rc != 0 {
				Warnf("[ZT] processBackgroundTasks rc=%d", int(rc))
			}
			next = newDeadline
			// Merge anything published while the core was busy.
			e.bgMu.Lock()
			if e.bgDeadline > 0 && (next == 0 || e.bgDeadline < next) {
				next = e.bgDeadline
				e.bgDeadline = 0
			}
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
				e.ztFramesRx.Add(1)
			default:
				// consumer backlog — drop newest, keep the flow moving
				e.framesDropped.Add(1)
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
		if len(snap.Routes) > 0 {
			// 只有「网络 OK 且路由非空」才刷新 lastKnown：换网掉线期间不会用
			// 一份空表把历史覆盖掉。
			e.lastRoutes.Set(snap.Routes)
		}
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

// ---- [ZT-SOCKET] 物理通道自愈（2026-11-17 现场驱动）----
//
// 现场：手机从办公室 WiFi 走到楼梯间切蜂窝、再回来，ZT 的 UDP socket 仍绑着
// 旧的本地端口 / 旧 NAT 映射。短时间切换（映射未过期）能自己恢复；一旦离开
// 久到 NAT 映射过期就再也打不了洞：i3 侧 `zerotier-cli peers` 一直挂着过期的
// DIRECT 状态（假在线），实测 ping 0/20、3 分钟零收包，只能重启 App 才能恢复。
//
// 两条自愈路径：
//  1. 主动：RebuildSocket 热替换物理 socket（node / identity / 成员状态 / 路由
//     全部保留，不需要重新授权，也不碰 TUN）。
//  2. 被动：socketWatchdog 检测「有出站、零入站」的僵死通道，自动重建一次。
//
// 判据特意选「有出站但零入站」而不是「零入站」：熄屏 / 无流量时本来就收不到
// 包，单看零入站会误伤。

const (
	socketWatchInterval = 5 * time.Second
	socketDeadTimeout   = 60 * time.Second // 连续 60 秒零入站且期间有出站 → 判死
	socketFixMinGap     = 60 * time.Second // 两次自愈的最小间隔，防风暴
	socketFixMax        = 10               // 单个 engine 生命周期内的自愈上限

	// socketRecoveryWindow 是重建后观察“物理接收是否恢复”的窗口。
	// 重建成功只说明 bind+protect+换 fd 成功，不代表链路恢复。
	socketRecoveryWindow = 30 * time.Second

	// 数据面僵死（frames frozen）：物理层还在收包，但送进 overlay 的业务帧
	// 拿不到任何回帧。2026-11-19 实机：WiFi→蜂窝后旧 socket 的 sendto 被
	// 静默丢弃，而对端 / 根服务器的 keepalive 照样收得进来（wire rx 每 10s
	// 还在涨），于是「有出站、零入站」永远判不到。
	//
	// 出站需求只看 SendFrame 成功次数（真正的业务帧），不看 keepalive：否则
	// 手机静置、没有 overlay 流量时也会被当成僵死，导致无休止地重启。
	socketFrameStallTimeout   = 20 * time.Second
	socketFrameStallMinGap    = 30 * time.Second // 两次数据面自愈的最小间隔
	socketFrameStallMax       = 3                // 单个 engine 生命周期内的自愈上限
	socketFrameStallMinFrames = 3                // 至少送过这么多帧才算“确实在等回应”

	// 换 socket 无效时的下一档：重建整个 ZT Node（真正的 node_delete +
	// node_new，Engine 指针与 frames channel 保持不变，所以 flow router
	// 不用重建）。2026-11-19 实机证据：rebind 之后 frames 仍可冻结 39~80s，
	// 而完整进程重启（= 重建 node）立刻恢复。
	//
	// ⚠️ 这些数值是初始实验值，未实测校准（设备侧没有可对照的数据）。
	socketFrameNodeRestartFrozenFor = 45 * time.Second  // 冻结超过这么久才升级
	socketFrameNodeRestartMinGap    = 120 * time.Second // 两次 node 重建的最小间隔
	socketFrameNodeRestartMax       = 2                 // 单个 engine 生命周期内的上限
)

// 链路健康判定值定义在 status.go（无 build tag，nocgo 构建也要用）。
// 判定**逻辑**只在 watchdog 的 checkSocketHealth 里实现一次，
// GetRuntimeStatus 直接读结果，Dart 侧不得重复实现阈值。

// SocketGen 返回当前物理 socket 世代号（bind 之前为 0）。
func (e *Engine) SocketGen() uint64 { return e.socketGen.Load() }

// NodeGen 返回 ZT Node 被创建的次数（1 = 首次启动，2+ = 中途重建过）。
func (e *Engine) NodeGen() uint64 { return e.nodeGen.Load() }

// nodeRestartStats 返回（为数据面僵死重建 node 的次数, 上限）。
func (e *Engine) nodeRestartStats() (restarts, max int) {
	e.wdMu.Lock()
	defer e.wdMu.Unlock()
	return e.wdNodeRestarts, socketFrameNodeRestartMax
}

// LinkStats 是网络页需要的链路层视图。
//
// 判定字符串来自 watchdog 每 5s 的采样结果（同一套常量、同一处实现），
// SilentMs / WireTxSinceRx 用同一批 wd 字段实时重算，所以两次采样之间也能
// 看到数字在动。watchdog 还没采样过（引擎刚起 <socketWatchInterval）时
// Health = unknown、两个指针为 nil —— 页面必须显示「—」，不能用 0 冒充。
type LinkStats struct {
	Health        string
	SilentMs      *int64
	WireTxSinceRx *uint64
	WireRxPkts    uint64
	// 数据面视角（ZT 帧）：帧多久没前进了、为此重建过几次 socket。
	// FramesFrozenMs 不因 socket 重建而清零——上层靠它决定要不要重启核心。
	FramesFrozenMs *int64
	FrameStalls    *int
	// ZT Node 世代与为数据面僵死重建 node 的次数（诊断用：socketGen 涨而
	// nodeGen 不动 = 只换了 socket，node 从没重建）。
	NodeGen      uint64
	NodeRestarts *int
}

func (e *Engine) linkStats(now time.Time) LinkStats {
	stats := LinkStats{Health: healthUnknown, WireRxPkts: e.wireRxPkts.Load()}
	rebuilding := e.rebuilding.Load()
	if rebuilding {
		stats.Health = healthRebuilding
	}
	txDemand := e.sendDemand() // 与 watchdog 同一口径：失败也算出站需求

	e.wdMu.Lock()
	defer e.wdMu.Unlock()
	if e.wdLastRxAt.IsZero() {
		return stats
	}
	silent := now.Sub(e.wdLastRxAt).Milliseconds()
	if silent < 0 {
		silent = 0
	}
	txSinceRx := txDemand - e.wdTxAtRx
	if !rebuilding && e.wdHealth != "" {
		stats.Health = e.wdHealth
	}
	stats.SilentMs = &silent
	stats.WireTxSinceRx = &txSinceRx
	if !e.wdFrameAt.IsZero() {
		frozen := now.Sub(e.wdFrameAt).Milliseconds()
		if frozen < 0 {
			frozen = 0
		}
		stalls := e.wdFrameStalls
		stats.FramesFrozenMs = &frozen
		stats.FrameStalls = &stalls
	}
	restarts := e.wdNodeRestarts
	stats.NodeGen = e.nodeGen.Load()
	stats.NodeRestarts = &restarts
	return stats
}

// socketStats 返回（自愈尝试次数, socket 重建成功次数, 上限）。
// 尝试 = watchdog 发起重建的次数；重建成功 = RebuildSocket 返回 nil 的次数。
// ⚠️ 重建成功**不等于**链路恢复，后者看 linkRecovery()。
func (e *Engine) socketStats() (attempts, rebuilds, max int) {
	e.wdMu.Lock()
	defer e.wdMu.Unlock()
	return e.wdFixes, e.wdFixOK, socketFixMax
}

// PeerSnapshot is the whole peer-plane answer: the rows plus whether the core
// had more rows than the wrapper's fixed capacity.
type PeerSnapshot struct {
	Peers     []PeerStatus
	Known     bool // false = no live node / query failed → 未知，不是“0 个”
	Truncated bool // true = there were more peers than we copied
	Total     int  // real core count when truncated, else len(Peers)
}

// PeerSnapshot 拉一次 ZT_Node_peers() 快照。
func (e *Engine) PeerSnapshot() PeerSnapshot {
	var raw C.flclashtier_zt_peer_list
	if C.flclashtier_zt_peers_export(&raw) != 1 || raw.ok != 1 {
		return PeerSnapshot{}
	}
	n := int(raw.count)
	if n > len(raw.peers) {
		n = len(raw.peers)
	}
	peers := make([]PeerStatus, 0, n)
	for i := 0; i < n; i++ {
		p := raw.peers[i]
		peers = append(peers, PeerStatus{
			Address:   formatNodeAddress(uint64(p.address)),
			LatencyMs: int(p.latencyMs),
			Version:   formatPeerVersion(int(p.versionMajor), int(p.versionMinor), int(p.versionRev)),
			Role:      peerRoleString(int32(p.role)),
			// Path 固定 unknown：见 status.go 里 PeerPathUnknown 的说明——
			// core 的 C API 不提供可靠的 direct/relay 证据。
			Path:            PeerPathUnknown,
			PathCount:       int(p.pathCount),
			PreferredPaths:  int(p.preferredPaths),
			PathsWithRx:     int(p.pathsWithRx),
			LastReceiveAtMs: int64(p.lastReceiveMs),
			LastSendAtMs:    int64(p.lastSendMs),
		})
	}
	snap := PeerSnapshot{Peers: peers, Known: true, Total: len(peers)}
	if raw.truncated == 1 {
		snap.Truncated = true
		if int(raw.peerCount) > snap.Total {
			snap.Total = int(raw.peerCount)
		}
	}
	return snap
}

// RebuildSocket 关闭当前物理 UDP socket，在同一本地端口重新 bind、重新
// protect，并把新 fd 交给 ZT core。
//
// 为什么换 fd 能抵达 core：C wrapper 把物理 fd 放在全局
// （flclashtier_zt_set_socket_fd），发包路径不依赖每个 Path 的 localSocket
// 取值，所以替换全局 fd 后后续所有 wirePacketSendFunction 都从新 socket 出去。
func (e *Engine) RebuildSocket(reason string) error {
	if !e.rebuilding.CompareAndSwap(false, true) {
		return errors.New("socket rebuild already in progress")
	}
	defer e.rebuilding.Store(false)

	e.mu.Lock()
	if e.state != StateRunning {
		st := e.state
		e.mu.Unlock()
		return fmt.Errorf("engine not running (state=%s)", st)
	}
	protect := e.protectFn
	old := e.udpConn.Swap(nil) // receiveLoop 见到 nil 会等待新 conn
	port := cfgPort(e.cfg)
	e.mu.Unlock()

	if old != nil {
		if la, ok := old.LocalAddr().(*net.UDPAddr); ok && la.Port != 0 {
			port = la.Port
		}
		_ = old.Close()
	}

	conn, err := bindPort(port)
	if err != nil {
		// 端口可能仍被内核短暂占用：交给 watchdog 下一轮再试。
		Warnf("[ZT-SOCKET] rebind port %d failed: %v", port, err)
		return fmt.Errorf("rebind UDP port %d: %w", port, err)
	}
	fd, err := socketFd(conn)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("udpConn.SyscallConn: %w", err)
	}
	if protect != nil && !protect(fd) {
		_ = conn.Close()
		return fmt.Errorf("protect(fd=%d) failed on rebind: socket would loop back into our own TUN", fd)
	}
	C.flclashtier_zt_set_socket_fd(C.int(fd))
	e.udpConn.Store(conn)
	// IPv6 侧跟着一起换：旧 fd 绑在失效网络上时，v6 的 NAT 映射同样会死。
	if old6 := e.udpConn6.Swap(nil); old6 != nil {
		_ = old6.Close()
	}
	e.attachSocket6(port, protect)
	gen := e.socketGen.Add(1)
	// 换网后 core 仍握着对端的旧路径：对端端点没变，路径看起来还有效，于是它
	// 继续往死路发、不做 rendezvous，直到 ZT 内部路径过期（~243s，真机实证
	// 241s 完全吻合）。这里让 core 立刻把旧路径标为不可用并重新 HELLO —— 效果
	// 等价于 0.9.14 的「重建 node = 冷启动」，但不销毁 node / identity /
	// managed routes，所以不会出现路由清空窗口。
	e.logPeerPaths("before-reset")
	if rc := C.flclashtier_zt_reset_peer_paths(0); rc != 0 {
		Warnf("[ZT-SOCKET] reset peer paths failed: rc=%d (continuing)", int(rc))
	} else {
		Infof("[ZT-SOCKET] peer paths reset after rebind: re-rendezvous forced")
		e.logPeerPaths("after-reset")
	}
	Infof("[ZT-SOCKET] rebind gen=%d addr=%s fd=%d reason=%q", gen, conn.LocalAddr().String(), fd, reason)
	e.DiagSnapshot("socket-rebind")
	return nil
}

// cfgPort 与 start() 的端口选择保持一致。
func cfgPort(cfg Config) int {
	if cfg.Port == 0 {
		return DefaultPort
	}
	return cfg.Port
}

// sendDemand 统计「本机想发出去」的次数：成功 + 各类失败。
//
// 为什么不能只看成功数：Android 换网（WiFi↔蜂窝）后旧 UDP socket 的
// sendto 会直接失败（ENETUNREACH 等），只有 err 计数在动、ok 计数冻结。
// 若用 ok 当需求，watchdog 会把这种「发不出去」判成 idle（无出站需求），
// 永远不重建 —— 2026-11-19 实测换到蜂窝后 4 分钟不自愈就是这个原因。
func (e *Engine) sendDemand() uint64 {
	_, ok, eagain, enobufs, other := e.DataPathCounters()
	return ok + eagain + enobufs + other
}

// RebuildActiveSocket 让当前运行的 Engine 立刻换一个物理 UDP socket。
//
// Android 的 UDP socket 跟随创建时的默认网络：换网后旧 fd 既发不出去也
// 收不到，只能重建。Dart 侧在 connectivityChanged 时主动调用；
// watchdog 的「零入站」判定是兜底。没有运行中的 Engine 时返回 nil。
func RebuildActiveSocket(reason string) error {
	e := currentEngine()
	if e == nil {
		return nil
	}
	if e.getState() != StateRunning {
		return nil
	}
	return e.RebuildSocket(reason)
}

// ErrNodeRestartRateLimited 表示换网触发的 node 重建被限速跳过（调用方应退回
// 只换 socket），与「重建失败」区分开。
var ErrNodeRestartRateLimited = errors.New("node restart rate-limited (45s window)")

// RestartActiveNodeForNetworkChange 在**物理网络变化**时重建整个 ZT Node。
//
// 为什么要重建 node 而不只是换 socket（2026-11-25 真机 + i3 抓包实证）：
//
//	冷启动（新 node、没有任何 peer path）连上 ZT 只要 <30s；
//	而换网后要 4 分钟才恢复。差别不在 socket，而在**对端路径**：
//	换网前节点和 i3 之间有一条「有效」路径（i3 的端点压根没变），
//	节点于是继续往那条死路发，不会去做 rendezvous/HELLO；
//	只有等路径 lastReceive 过期（ZT 内部 ~2-4 分钟）才会重新打洞。
//	新 node 没有历史路径 → 立刻 rendezvous → 秒级建立（含 IPv6 直连）。
//
// 所以换网时直接把路径清干净 = 人为制造一次「冷启动」。
// 限速：45s 内最多一次（换网回调可能连发好几次），并且只在 RUNNING 时做。
func RestartActiveNodeForNetworkChange(reason string) error {
	e := currentEngine()
	if e == nil {
		return nil
	}
	if e.getState() != StateRunning {
		return nil
	}
	if !e.markNetworkNodeRestart() {
		// 必须让调用方知道「没做」，否则 Dart 会当成成功、跳过 socket 兜底
		// （2026-11-25 评审指出的假成功）。
		return ErrNodeRestartRateLimited
	}
	return e.RestartNode(reason)
}

// socketWatchdog 每 5 秒采样一次物理层计数，发现「有出站、零入站」超过
// socketDeadTimeout 就重建 socket。
func (e *Engine) socketWatchdog() {
	defer e.wg.Done()
	t := time.NewTicker(socketWatchInterval)
	defer t.Stop()
	for {
		select {
		case <-e.stopCh:
			return
		case <-t.C:
			now := time.Now()
			e.checkSocketHealth(now)
			e.diagPeriodic(now)
		}
	}
}

// judgeSocketHealth 是链路健康的唯一判据。阈值全部来自上面的 watchdog
// 常量，本函数不含新的魔法数，也不被 Dart 重复实现。
//
//	rebuilding  重建进行中（优先级最高）
//	sampled     watchdog 是否已经采过样（wdLastRxAt 非零）
//	silent      距上次入站的时长
//	txSinceRx   这段时间里发出去的包数
//	fixes       已尝试的自愈次数
//	fixGap      距上次自愈的间隔；负数 = 从未自愈过
//
// 返回 healthUnknown / healthIdle / healthOK / healthStalled，以及
// healthRebuilding（这里同时表示“已判死且此刻允许重建”，由调用方执行）。
func judgeSocketHealth(rebuilding, sampled bool, silent time.Duration, txSinceRx uint64, fixes int, fixGap time.Duration) string {
	switch {
	case rebuilding:
		return healthRebuilding
	case !sampled:
		return healthUnknown
	case txSinceRx == 0:
		// 没有出站需求（熄屏 / 无流量）：单看零入站不能判死
		return healthIdle
	case silent < socketDeadTimeout:
		return healthOK
	case fixes >= socketFixMax:
		return healthStalled
	case fixGap >= 0 && fixGap < socketFixMinGap:
		return healthStalled // 已判死，但自愈间隔未到
	default:
		return healthRebuilding // 已判死且允许重建
	}
}

func (e *Engine) checkSocketHealth(now time.Time) {
	if e.rebuilding.Load() {
		e.setHealth(healthRebuilding)
		return
	}
	// node 重建进行中：teardown 已关掉 socket，帧/入站计数在这段时间没有
	// 意义，再判一次只会产生噪声并可能叠加第二次重建。
	if e.restarting.Load() {
		e.setHealth(healthRebuilding)
		return
	}
	rx := e.wireRxPkts.Load()
	txDemand := e.sendDemand() // 失败也算需求，见 sendDemand 注释

	e.wdMu.Lock()
	// 采样循环可能被系统挂起（Android Doze / 长时间后台）：两次采样间隔远超
	// 正常周期就说明这段时间根本没有观测，不能拿旧的 wdLastRxAt 去判
	// “静默了三小时”。复位基线、健康值回到 unknown，下一轮重新采样。
	gap := time.Duration(-1)
	if !e.wdLastSample.IsZero() {
		gap = now.Sub(e.wdLastSample)
	}
	e.wdLastSample = now
	if gap > 3*socketWatchInterval {
		e.wdLastRxPkt = rx
		e.wdLastRxAt = now
		e.wdTxAtRx = txDemand
		e.wdHealth = healthUnknown
		e.wdMu.Unlock()
		return
	}
	e.evalRecoveryLocked(rx, now)
	if e.handleFrameStallLocked(e.ztFramesRx.Load(), e.ztFramesTx.Load(), now) {
		// 数据面僵死已处理（内部已解锁并可能重建 socket）。这种僵死的特征
		// 恰恰是「入站一直在涨」，继续走下面的入站判据只会判成健康。
		return
	}
	if rx > e.wdLastRxPkt || e.wdLastRxAt.IsZero() {
		e.wdLastRxPkt = rx
		e.wdLastRxAt = now
		e.wdTxAtRx = txDemand
		e.wdHealth = healthOK
		e.wdMu.Unlock()
		return
	}
	silent := now.Sub(e.wdLastRxAt)
	txSinceRx := txDemand - e.wdTxAtRx
	fixGap := time.Duration(-1)
	if !e.wdLastFix.IsZero() {
		fixGap = now.Sub(e.wdLastFix)
	}
	fixes := e.wdFixes
	health := judgeSocketHealth(false, true, silent, txSinceRx, fixes, fixGap)
	if health != healthRebuilding {
		e.wdHealth = health
		e.wdMu.Unlock()
		return
	}

	e.wdLastFix = now
	e.wdFixes++        // 尝试次数：重建成不成功都先计，成功数在下面单独计
	e.wdLastRxAt = now // 给重建后的新 socket 一个完整观察窗口
	e.wdTxAtRx = txDemand
	e.wdHealth = healthRebuilding
	e.wdMu.Unlock()

	Warnf("[ZT-SOCKET] watchdog: no inbound for %s while %d outbound packets were sent -> rebuilding socket (fix #%d)",
		silent.Round(time.Second), txSinceRx, fixes+1)
	if err := e.RebuildSocket("watchdog: inbound stalled"); err != nil {
		Warnf("[ZT-SOCKET] watchdog rebuild failed: %v", err)
		e.setHealth(healthStalled)
		return
	}
	e.wdMu.Lock()
	e.wdFixOK++
	// 重建成功 ≠ 链路恢复：开一个观察窗口，等新的 UDP 入站来确认。
	e.wdRecoveryGen = e.socketGen.Load()
	e.wdRecoveryRxBase = e.wireRxPkts.Load()
	e.wdRecoveryUntil = time.Now().Add(socketRecoveryWindow)
	e.wdRecoveryDone = false
	e.wdRecoveryState = "pending"
	e.wdMu.Unlock()
}

// evalRecoveryLocked 判定上一次 socket 重建是否真的让物理层恢复。
// 调用方必须持有 wdMu。
func (e *Engine) evalRecoveryLocked(rx uint64, now time.Time) {
	if e.wdRecoveryDone || e.wdRecoveryUntil.IsZero() {
		return
	}
	if rx > e.wdRecoveryRxBase {
		e.wdRecoveryDone = true
		e.wdRecoveryState = "recovered"
		e.wdRecoveryOK++
		return
	}
	if now.After(e.wdRecoveryUntil) {
		e.wdRecoveryDone = true
		// 无入站证据 ≠ 恢复失败：记录 inconclusive，不计入失败。
		e.wdRecoveryState = "inconclusive"
		e.wdRecoveryInconclusive++
	}
}

// linkRecovery 返回上一次重建后的恢复确认状态与计数。
func (e *Engine) linkRecovery() (state string, recovered, inconclusive int) {
	e.wdMu.Lock()
	defer e.wdMu.Unlock()
	state = e.wdRecoveryState
	if state == "" {
		state = "none"
	}
	return state, e.wdRecoveryOK, e.wdRecoveryInconclusive
}

func (e *Engine) setHealth(health string) {
	e.wdMu.Lock()
	e.wdHealth = health
	e.wdMu.Unlock()
}
