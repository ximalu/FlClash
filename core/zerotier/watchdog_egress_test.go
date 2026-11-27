//go:build cgo

package zerotier

import (
	"testing"
	"time"
)

func TestJudgeEgressProgress(t *testing.T) {
	cases := []struct {
		name     string
		snat     uint64
		dnat     uint64
		frozen   time.Duration
		resets   int
		resetGap time.Duration
		want     egressVerdict
	}{
		{"dnat advancing means alive", 100, 1, 5 * time.Minute, 0, -1, egressHealthy},
		{"not enough outbound demand", 3, 0, 5 * time.Minute, 0, -1, egressHealthy},
		{"frozen below timeout", 50, 0, 10 * time.Second, 0, -1, egressHealthy},
		{"dead outbound -> reset once", 50, 0, 21 * time.Second, 0, -1, egressResetPaths},
		{"just reset, still in window", 50, 0, 25 * time.Second, 1, 5 * time.Second, egressHealthy},
		{"second reset allowed after gap", 50, 0, 60 * time.Second, 1, 40 * time.Second, egressResetPaths},
		{"resets exhausted -> stalled", 50, 0, 60 * time.Second, egressResetMax, 40 * time.Second, egressStalled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := judgeEgressProgress(tc.snat, tc.dnat, tc.frozen, tc.resets, tc.resetGap)
			if got != tc.want {
				t.Fatalf("judgeEgressProgress() = %v, want %v", got, tc.want)
			}
		})
	}
}

// 静置手机（没有出站需求）时不能判死：snat 不涨就是证据不足。
func TestJudgeEgressProgressIdleDeviceStaysHealthy(t *testing.T) {
	if got := judgeEgressProgress(0, 0, 10*time.Minute, 0, -1); got != egressHealthy {
		t.Fatalf("idle device judged %v, want healthy", got)
	}
}

// 只复位「入向还活着」的 LEAF：这正是单向断链的特征。
func TestEgressTargetPeers(t *testing.T) {
	now := int64(1_000_000)
	peers := []PeerStatus{
		{Address: "a5cd13296e", Role: "LEAF", PathCount: 1, LastReceiveAtMs: now - 3_000},   // ✓ 刚收到
		{Address: "8efcdece80", Role: "LEAF", PathCount: 1, LastReceiveAtMs: now - 40_000},  // ✗ 太久没收到
		{Address: "778cde7190", Role: "PLANET", PathCount: 1, LastReceiveAtMs: now - 1_000}, // ✗ root 不动
		{Address: "b6079f73c6", Role: "LEAF", PathCount: 0, LastReceiveAtMs: now - 1_000},   // ✗ 没路径
		{Address: "cafe04eba9", Role: "MOON", PathCount: 1, LastReceiveAtMs: now - 1_000},   // ✗ 非 LEAF
	}
	got := egressTargetPeers(peers, now)
	if len(got) != 1 || got[0].Address != "a5cd13296e" {
		t.Fatalf("targets = %+v, want only a5cd13296e", got)
	}
}

// 双向全哑（没有任何 peer 还在发给我们）：不该乱复位，交给 socket 看门狗。
func TestEgressTargetPeersAllSilent(t *testing.T) {
	now := int64(1_000_000)
	peers := []PeerStatus{
		{Address: "a5cd13296e", Role: "LEAF", PathCount: 1, LastReceiveAtMs: now - 120_000},
	}
	if got := egressTargetPeers(peers, now); len(got) != 0 {
		t.Fatalf("targets = %+v, want none", got)
	}
}

// tickEgress 按 handleEgressStallLocked 的加锁契约调一次：
// 返回 false 时调用方仍持锁（需自己解），返回 true 时内部已解锁。
func tickEgress(e *Engine, now time.Time) bool {
	e.wdMu.Lock()
	handled := e.handleEgressStallLocked(now)
	if !handled {
		e.wdMu.Unlock()
	}
	return handled
}

// A 档（计数回退保护）：adapter 重建等场景会让 adapter 计数器归零。
// 若基线不跟着重建，snatSince = snat - wdEgressSnat 会在 uint64 上回绕成 ~1.8e19，
// 使「snat 已涨够 8 帧」恒成立 → 立刻误判出向失效并去复位 LEAF 路径。
//
// 三种回退形态都要被抓住，并且必须：完整重建基线、不进任何恢复分支、后续采样不重复告警。
func TestEgressCounterWentBackwards(t *testing.T) {
	cases := []struct {
		name                           string
		baseSnat, baseDnat, snat, dnat uint64
	}{
		{"only snat went backwards", 100, 50, 10, 50},
		{"only dnat went backwards", 100, 50, 100, 5},
		{"both went backwards", 100, 50, 10, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snat, dnat := tc.baseSnat, tc.baseDnat
			e := &Engine{}
			e.egressStats = func() (uint64, uint64) { return snat, dnat }

			// 第一次采样：只建立基线。
			t0 := time.Unix(1_700_000_000, 0)
			if tickEgress(e, t0) {
				t.Fatal("first sample must only establish the baseline")
			}
			if e.wdEgressSnat != tc.baseSnat || e.wdEgressDnat != tc.baseDnat {
				t.Fatalf("baseline = (%d,%d), want (%d,%d)",
					e.wdEgressSnat, e.wdEgressDnat, tc.baseSnat, tc.baseDnat)
			}

			// 模拟「刚刚做过一次 peer reset」：回退不得借机绕过恢复频率限制。
			priorResetAt := t0.Add(-3 * time.Second)
			e.wdMu.Lock()
			e.wdEgressResets = 1
			e.wdEgressResetAt = priorResetAt
			e.wdMu.Unlock()

			// 计数器归零后的第一次采样（adapter 重建 / 计数归零）。
			snat, dnat = tc.snat, tc.dnat
			t1 := t0.Add(5 * time.Second)
			if tickEgress(e, t1) {
				t.Fatal("counter rollback must not run any recovery action")
			}
			if e.wdEgressSnat != tc.snat || e.wdEgressDnat != tc.dnat {
				t.Fatalf("baseline not rebuilt: (%d,%d), want (%d,%d)",
					e.wdEgressSnat, e.wdEgressDnat, tc.snat, tc.dnat)
			}
			if !e.wdEgressAt.Equal(t1) {
				t.Fatalf("freeze clock not rebuilt: %v, want %v", e.wdEgressAt, t1)
			}
			if e.wdEgressFrozenMs != 0 {
				t.Fatalf("freeze duration not cleared: %d", e.wdEgressFrozenMs)
			}
			// 恢复动作记录必须保留：回退不是“新世代”，不得借机重置次数上限与冷却窗。
			if e.wdEgressResets != 1 {
				t.Fatalf("reset counter bypassed: %d, want 1 (kept)", e.wdEgressResets)
			}
			if !e.wdEgressResetAt.Equal(priorResetAt) {
				t.Fatalf("reset cooldown bypassed: %v, want %v (kept)",
					e.wdEgressResetAt, priorResetAt)
			}
			if e.wdEgressBackwards != 1 {
				t.Fatalf("backwards count = %d, want 1", e.wdEgressBackwards)
			}

			// 重建后的下一次正常采样：不得再次触发回退。
			snat, dnat = tc.snat+5, tc.dnat+1
			t2 := t1.Add(5 * time.Second)
			if tickEgress(e, t2) {
				t.Fatal("normal sample must not run a recovery action")
			}
			if e.wdEgressBackwards != 1 {
				t.Fatalf("backwards count after normal sample = %d, want 1",
					e.wdEgressBackwards)
			}
			if e.wdEgressDnat != tc.dnat+1 || !e.wdEgressAt.Equal(t2) {
				t.Fatalf("dnat advance not observed: dnat=%d at=%v",
					e.wdEgressDnat, e.wdEgressAt)
			}
		})
	}
}

// 正常单调增长的计数器永远不能进入回退分支（A 档不得改变既有行为）。
func TestEgressMonotonicCountersNeverBackwards(t *testing.T) {
	snat, dnat := uint64(0), uint64(0)
	expSnat, expDnat := uint64(0), uint64(0) // 「最近一次 dnat 前进时」的快照
	e := &Engine{}
	e.egressStats = func() (uint64, uint64) { return snat, dnat }
	t0 := time.Unix(1_700_000_000, 0)
	for i := 0; i < 10; i++ {
		snat += 3
		if i%2 == 0 {
			dnat++
			expSnat, expDnat = snat, dnat
		}
		if tickEgress(e, t0.Add(time.Duration(i)*5*time.Second)) {
			t.Fatalf("sample %d unexpectedly ran a recovery action", i)
		}
	}
	if e.wdEgressBackwards != 0 {
		t.Fatalf("backwards count = %d, want 0", e.wdEgressBackwards)
	}
	// 基线语义 = 「最近一次 dnat 前进时」的快照：snat 只在 dnat 前进时随基线更新，
	// 所以最后一轮 dnat 未前进时 wdEgressSnat 会停在上一轮的值 —— 这是刻意的，不是漂移。
	if e.wdEgressSnat != expSnat || e.wdEgressDnat != expDnat {
		t.Fatalf("baseline = (%d,%d), want snapshot-at-last-dnat-advance (%d,%d)",
			e.wdEgressSnat, e.wdEgressDnat, expSnat, expDnat)
	}
}

// socketGen 变化仍按原行为重建基线（A 档不改 rebind 判定）：
// 不能因为新增了回退检测就把 rebind 的基线重建算成一次「回退」。
func TestEgressRebindRebaselinesWithoutBackwards(t *testing.T) {
	snat, dnat := uint64(100), uint64(50)
	e := &Engine{}
	e.egressStats = func() (uint64, uint64) { return snat, dnat }
	t0 := time.Unix(1_700_000_000, 0)
	if tickEgress(e, t0) {
		t.Fatal("first sample must only establish the baseline")
	}

	// 换 socket：计数器单调继续（adapter 未重建）。
	e.socketGen.Add(1)
	snat, dnat = 120, 60
	t1 := t0.Add(5 * time.Second)
	if tickEgress(e, t1) {
		t.Fatal("rebind must not run a recovery action")
	}
	if e.wdEgressBackwards != 0 {
		t.Fatalf("rebind counted as a counter rollback: %d", e.wdEgressBackwards)
	}
	if e.wdEgressSnat != snat || e.wdEgressDnat != dnat {
		t.Fatalf("rebind baseline = (%d,%d), want (%d,%d)",
			e.wdEgressSnat, e.wdEgressDnat, snat, dnat)
	}
}

// A 档不得借计数器回退绕过恢复频率限制：
// wdEgressResetAt 是「恢复动作的冷却窗」（egressResetMinGap = 30s），
// wdEgressResets 是次数上限（egressResetMax = 2）—— 它们属于**恢复动作的生命周期**，
// 不属于计数器观测基线。回退时一并清零会形成「回退→清零→立刻复位→回退」的循环。
func TestEgressBackwardsDoesNotBypassResetCooldown(t *testing.T) {
	snat, dnat := uint64(100), uint64(50)
	e := &Engine{}
	e.egressStats = func() (uint64, uint64) { return snat, dnat }

	t0 := time.Unix(1_700_000_000, 0)
	if tickEgress(e, t0) {
		t.Fatal("first sample must only establish the baseline")
	}

	// 此刻刚刚做过一次 peer reset。
	e.wdMu.Lock()
	e.wdEgressResets = 1
	e.wdEgressResetAt = t0
	e.wdMu.Unlock()

	// t0+5s：计数器归零（adapter 重建）。观测基线重建，但恢复动作记录必须保留。
	snat, dnat = 5, 5
	t1 := t0.Add(5 * time.Second)
	if tickEgress(e, t1) {
		t.Fatal("counter rollback must not run a recovery action")
	}
	if e.wdEgressResets != 1 || !e.wdEgressResetAt.Equal(t0) {
		t.Fatalf("recovery record bypassed: resets=%d resetAt=%v",
			e.wdEgressResets, e.wdEgressResetAt)
	}

	// t0+25s：snat 已涨 25 帧、dnat 冻了 20s —— 判死条件齐了，
	// 但离上次复位只有 25s < 30s 冷却窗 ⇒ 必须判健康、不得复位。
	snat = 30
	t2 := t0.Add(25 * time.Second)
	if tickEgress(e, t2) {
		t.Fatal("cooldown window must suppress the reset (rollback must not bypass it)")
	}
	if e.wdEgressResets != 1 {
		t.Fatalf("resets = %d, want 1 (cooldown must not consume a slot)", e.wdEgressResets)
	}

	// t0+35s：冷却窗已过 ⇒ 允许复位一次（证明保留记录不会阻塞正常恢复）。
	snat = 60
	t3 := t0.Add(35 * time.Second)
	if !tickEgress(e, t3) {
		t.Fatal("after the cooldown a peer reset must be allowed again")
	}
}
