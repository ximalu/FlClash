//go:build linux && !android && cgo

package zerotier

// Node restart tests (2026-11-19). Background: on a WiFi→cellular switch the
// 2026-11-19 device log showed socket rebinds (gen=2, gen=20) that never
// restored frames, while a full process restart did. That means "replace the
// fd" is not enough — the whole ZT node has to be rebuilt. These tests pin the
// Go-side restart so it can be verified on i3 instead of on the phone:
//
//   TestG1: RestartNode keeps the SAME *Engine and frames channel (the flow
//           router / adapter hold that pointer and channel), creates a NEW
//           C node (nodeGen 1→2), keeps identity + bound port, stays RUNNING.
//   TestG2: several restarts in a row — goroutines and port stay stable,
//           no leak, no double free.
//   TestG3: Stop after a restart releases the global slot (a later StartEngine
//           must build a fresh engine, not reuse a half-dead one).
//   TestG4: protect() returning false is fail-closed: StartEngine fails and
//           leaves no engine behind.
//   TestG5: concurrent RestartNode calls are serialized by the restarting CAS.

import (
	"sync"
	"testing"
	"time"
)

func TestG1_RestartNodeRecreatesNode(t *testing.T) {
	port := pickFreePort(t)
	e, err := StartEngine(testConfig(port), nil, t.TempDir())
	if err != nil {
		t.Fatalf("StartEngine failed: %v", err)
	}
	defer e.Stop()

	addrBefore := e.nodeAddress()
	genBefore := e.NodeGen()
	socketGenBefore := e.SocketGen()
	framesBefore := e.Frames()

	if genBefore != 1 {
		t.Fatalf("nodeGen after first start = %d, want 1", genBefore)
	}
	if err := e.RestartNode("test"); err != nil {
		t.Fatalf("RestartNode failed: %v", err)
	}
	if currentEngine() != e {
		t.Fatal("RestartNode replaced the engine; flow router would hold a stale pointer")
	}
	if got := e.getState(); got != StateRunning {
		t.Fatalf("state after restart = %v, want RUNNING", got)
	}
	if got := e.NodeGen(); got != genBefore+1 {
		t.Fatalf("nodeGen after restart = %d, want %d", got, genBefore+1)
	}
	if got := e.SocketGen(); got <= socketGenBefore {
		t.Fatalf("socketGen after restart = %d, want > %d", got, socketGenBefore)
	}
	if e.Frames() != framesBefore {
		t.Fatal("frames channel changed across restart; flow router frame loop would be orphaned")
	}
	if got := e.nodeAddress(); got != addrBefore {
		t.Fatalf("node address changed across restart: %010x -> %010x (identity not persisted?)", addrBefore, got)
	}
	if got := e.boundPort(); got != port {
		t.Fatalf("bound port after restart = %d, want %d (endpoint must stay stable)", got, port)
	}
}

func TestG2_RepeatedRestart(t *testing.T) {
	port := pickFreePort(t)
	e, err := StartEngine(testConfig(port), nil, t.TempDir())
	if err != nil {
		t.Fatalf("StartEngine failed: %v", err)
	}
	defer e.Stop()

	baseGoroutines := countGoroutines()
	for i := 0; i < 3; i++ {
		if err := e.RestartNode("test loop"); err != nil {
			t.Fatalf("RestartNode #%d failed: %v", i+1, err)
		}
		if got := e.boundPort(); got != port {
			t.Fatalf("restart #%d: bound port = %d, want %d", i+1, got, port)
		}
	}
	if got := e.NodeGen(); got != 4 {
		t.Fatalf("nodeGen after 3 restarts = %d, want 4", got)
	}
	// 慢退出可能还在收尾，给一点余量再比 goroutine 数。
	time.Sleep(200 * time.Millisecond)
	if after := countGoroutines(); after > baseGoroutines+2 {
		t.Fatalf("goroutine leak after restarts: %d -> %d", baseGoroutines, after)
	}
}

func TestG3_StopAfterRestartReleasesSlot(t *testing.T) {
	port := pickFreePort(t)
	e, err := StartEngine(testConfig(port), nil, t.TempDir())
	if err != nil {
		t.Fatalf("StartEngine failed: %v", err)
	}
	if err := e.RestartNode("test"); err != nil {
		t.Fatalf("RestartNode failed: %v", err)
	}
	e.Stop()
	if got := e.getState(); got != StateStopped {
		t.Fatalf("state after stop = %v, want STOPPED", got)
	}
	if !WaitEngineStopped(2 * time.Second) {
		t.Fatal("global engine slot not released after Stop")
	}
	if currentEngine() != nil {
		t.Fatal("currentEngine() != nil after Stop")
	}
	// 槽位释放后必须能建一个全新的 engine（不能复用死掉的旧对象）。
	e2, err := StartEngine(testConfig(port), nil, t.TempDir())
	if err != nil {
		t.Fatalf("StartEngine after stop failed: %v", err)
	}
	if e2 == e {
		t.Fatal("StartEngine reused the stopped engine")
	}
	e2.Stop()
}

func TestG4_ProtectFailureIsFatal(t *testing.T) {
	port := pickFreePort(t)
	// Android 上 protect 失败意味着 socket 仍会被自己的 TUN 捕获（ZT UDP 打环）。
	// engine 必须拒绝启动，而不是带着一个坏 socket 继续跑。
	_, err := StartEngine(testConfig(port), func(int) bool { return false }, t.TempDir())
	if err == nil {
		t.Fatal("StartEngine succeeded although protect() failed; expected fail-closed")
	}
	if currentEngine() != nil {
		t.Fatal("a failed start left an engine in the global slot")
	}
	if !WaitEngineStopped(2 * time.Second) {
		t.Fatal("global slot not released after failed start")
	}
	// 端口必须已经释放（abort 关了 conn），否则下一次启动会 EADDRINUSE。
	e, err := StartEngine(testConfig(port), nil, t.TempDir())
	if err != nil {
		t.Fatalf("StartEngine after protect failure failed: %v", err)
	}
	e.Stop()
}

func TestG5_ConcurrentRestartSerialized(t *testing.T) {
	port := pickFreePort(t)
	e, err := StartEngine(testConfig(port), nil, t.TempDir())
	if err != nil {
		t.Fatalf("StartEngine failed: %v", err)
	}
	defer e.Stop()

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			errs[idx] = e.RestartNode("test concurrent")
		}(i)
	}
	wg.Wait()

	succeeded := 0
	for _, err := range errs {
		if err == nil {
			succeeded++
		}
	}
	if succeeded == 0 {
		t.Fatal("no concurrent restart succeeded")
	}
	if got := e.getState(); got != StateRunning {
		t.Fatalf("state after concurrent restarts = %v, want RUNNING", got)
	}
	if got := e.boundPort(); got != port {
		t.Fatalf("bound port after concurrent restarts = %d, want %d", got, port)
	}
}

// TestH_WatchdogEscalationChain 是「换网僵死」的 i3 侧端到端模拟：
// 不碰真机网络，而是按实机日志的形状直接构造冻结状态，然后让**真实的**
// watchdog 判据在主循环里跑一遍，验证三档升级真的会依次发生：
//
//	frozen 25s（socket 档位可用）      → 真的 RebuildSocket（socketGen +1）
//	frozen 50s + socket 次数用尽        → 真的 RestartNode（nodeGen 1→2）
//	frozen 60s + node 次数也用尽        → health=stalled（交给上层整包重启）
//
// 2026-11-19 真机日志的证据是「rebind 之后 frames 仍为 0，只有杀进程恢复」，
// 这三步正是那次的补丁；在 i3 上用真实 core 跑一遍，比上手机按时刻重现便宜。
func TestH_WatchdogEscalationChain(t *testing.T) {
	port := pickFreePort(t)
	e, err := StartEngine(testConfig(port), nil, t.TempDir())
	if err != nil {
		t.Fatalf("StartEngine failed: %v", err)
	}
	defer e.Stop()

	// 构造「送过业务帧、但一直没有回帧」的形状（与实机 datapath 计数一致）。
	freeze := func(frozen time.Duration, rebuilds, nodeRestarts int) {
		e.wdMu.Lock()
		defer e.wdMu.Unlock()
		e.wdLastSample = time.Now().Add(-socketWatchInterval)
		e.wdLastRxPkt = e.wireRxPkts.Load()
		e.wdLastRxAt = time.Now()
		e.wdTxAtRx = e.sendDemand()
		e.ztFramesTx.Store(50) // 出站需求：50 个业务帧，零回帧
		e.wdFrameAt = time.Now().Add(-frozen)
		e.wdFrameMark = e.ztFramesRx.Load()
		e.wdFrameTxAt = 0
		e.wdFrameRebuilds = rebuilds
		e.wdNodeRestarts = nodeRestarts
		e.wdFrameFixAt = time.Time{}
		e.wdFrameNodeAt = time.Time{}
		e.wdHealth = healthOK
	}

	// 1) 换 socket 档位
	socketGenBefore := e.SocketGen()
	freeze(25*time.Second, 0, 0)
	e.checkSocketHealth(time.Now())
	if got := e.SocketGen(); got != socketGenBefore+1 {
		t.Fatalf("socketGen = %d after frame stall, want %d (socket rebuild not triggered)", got, socketGenBefore+1)
	}
	if got := e.NodeGen(); got != 1 {
		t.Fatalf("nodeGen = %d, want 1 (must not rebuild the node while socket fixes are still available)", got)
	}
	if got := e.boundPort(); got != port {
		t.Fatalf("bound port after rebind = %d, want %d", got, port)
	}

	// 2) 换 node 档位（socket 次数用尽）
	freeze(50*time.Second, socketFrameStallMax, 0)
	e.checkSocketHealth(time.Now())
	// 2026-11-24：node 重建改到**不进 wg 的 goroutine** 里跑（同步调用会触发
	// teardown → wg.Wait() 等待自己而死锁，1904 实机就是这样永久卡死的），
	// 所以这里必须等它完成，不能再假设 checkSocketHealth 返回时已经重建好。
	// nodeGen 在 start() 里自增（早于 RestartNode 末尾的 setState(RUNNING)），
	// 所以必须同时等状态回到 RUNNING，否则会读到 teardown 期间的 STOPPED。
	waitFor(t, 10*time.Second, func() bool {
		return e.NodeGen() == 2 && e.getState() == StateRunning && !e.restarting.Load()
	}, "nodeGen did not reach 2 / engine not RUNNING after node restart")
	if got := e.getState(); got != StateRunning {
		t.Fatalf("state after node restart = %v, want RUNNING", got)
	}
	if got := e.boundPort(); got != port {
		t.Fatalf("bound port after node restart = %d, want %d", got, port)
	}

	// 3) 两档都用尽 → stalled（由 Dart 侧整包重启兜底）
	freeze(60*time.Second, socketFrameStallMax, socketFrameNodeRestartMax)
	e.checkSocketHealth(time.Now())
	e.wdMu.Lock()
	health := e.wdHealth
	restarts := e.wdNodeRestarts
	e.wdMu.Unlock()
	if health != healthStalled {
		t.Fatalf("health = %q after exhausting all levels, want %q", health, healthStalled)
	}
	if restarts != socketFrameNodeRestartMax {
		t.Fatalf("node restarts = %d, want %d (must not exceed the cap)", restarts, socketFrameNodeRestartMax)
	}
}

// waitFor 轮询等待条件成立（用于异步动作，例如 2026-11-24 起放到独立
// goroutine 里的 node 重建）。超时即 Fatal。
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s (timeout %s)", msg, timeout)
}
