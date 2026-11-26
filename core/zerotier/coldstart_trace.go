//go:build cgo

package zerotier

import (
	"sync"
	"sync/atomic"
	"time"
)

// 冷启动时间线（Fast Network Recovery 实验，2026-11-27）
//
// 目的：把一次「换网后重建 node」拆成可测量的阶段，用同一份日志就能填出
// 「检测→销毁→建 node→拿到配置→首帧→首包业务数据」的时间表。只靠
// 「node ONLINE」判恢复是不够的 —— 用户关心的是能不能访问 i3。
//
// 日志形如：
//
//	[ZT-COLD] begin reason="connectivity changed"
//	[ZT-COLD] teardown done +312ms
//	[ZT-COLD] node up +905ms socketGen=… nodeGen=…
//	[ZT-COLD] config OK +6104ms rev=23 assigned=1 routes=1
//	[ZT-COLD] first overlay frame +6520ms
//	[ZT-COLD] first inbound data +7010ms dnat=3
//	[ZT-COLD] done total=7.0s (config 6.1s, frame 6.5s, data 7.0s)
//
// 为什么在 Go 侧做：Dart 只能看到「重启是否成功」，看不到 teardown / node up /
// 配置到位 / 首帧这几个真正决定体验的时刻。

type coldStartTrace struct {
	mu         sync.Mutex
	active     atomic.Bool
	reason     string
	t0         time.Time
	atTeardown time.Time
	atNodeUp   time.Time
	atConfig   time.Time
	atFrame    time.Time
	atData     time.Time
}

var coldStart coldStartTrace

const coldStartTraceTTL = 3 * time.Minute

func (t *coldStartTrace) begin(reason string) {
	t.mu.Lock()
	t.reason = reason
	t.t0 = time.Now()
	t.atTeardown = time.Time{}
	t.atNodeUp = time.Time{}
	t.atConfig = time.Time{}
	t.atFrame = time.Time{}
	t.atData = time.Time{}
	t.mu.Unlock()
	t.active.Store(true)
	Infof("[ZT-COLD] begin reason=%q", reason)
}

// elapsed 返回自 begin 起的时长；未开始时返回 -1。
func (t *coldStartTrace) elapsed(now time.Time) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.t0.IsZero() {
		return -1
	}
	return now.Sub(t.t0)
}

func (t *coldStartTrace) markTeardown() {
	if !t.active.Load() {
		return
	}
	t.mu.Lock()
	if t.t0.IsZero() || !t.atTeardown.IsZero() {
		t.mu.Unlock()
		return
	}
	t.atTeardown = time.Now()
	ms := t.atTeardown.Sub(t.t0).Milliseconds()
	t.mu.Unlock()
	Infof("[ZT-COLD] teardown done +%dms", ms)
}

func (t *coldStartTrace) markNodeUp(socketGen, nodeGen uint64) {
	if !t.active.Load() {
		return
	}
	t.mu.Lock()
	if t.t0.IsZero() || !t.atNodeUp.IsZero() {
		t.mu.Unlock()
		return
	}
	t.atNodeUp = time.Now()
	ms := t.atNodeUp.Sub(t.t0).Milliseconds()
	t.mu.Unlock()
	Infof("[ZT-COLD] node up +%dms socketGen=%d nodeGen=%d", ms, socketGen, nodeGen)
}

func (t *coldStartTrace) markConfig(rev uint64, assigned, routes int) {
	if !t.active.Load() {
		return
	}
	t.mu.Lock()
	if t.t0.IsZero() || !t.atConfig.IsZero() {
		t.mu.Unlock()
		return
	}
	t.atConfig = time.Now()
	ms := t.atConfig.Sub(t.t0).Milliseconds()
	t.mu.Unlock()
	Infof("[ZT-COLD] config OK +%dms rev=%d assigned=%d routes=%d", ms, rev, assigned, routes)
}

func (t *coldStartTrace) markFirstFrame() {
	if !t.active.Load() {
		return
	}
	t.mu.Lock()
	if t.t0.IsZero() || !t.atFrame.IsZero() {
		t.mu.Unlock()
		return
	}
	t.atFrame = time.Now()
	ms := t.atFrame.Sub(t.t0).Milliseconds()
	t.mu.Unlock()
	Infof("[ZT-COLD] first overlay frame +%dms", ms)
}

func (t *coldStartTrace) markFirstData(dnat uint64) {
	if !t.active.Load() {
		return
	}
	t.mu.Lock()
	if t.t0.IsZero() || !t.atData.IsZero() {
		t.mu.Unlock()
		return
	}
	t.atData = time.Now()
	ms := t.atData.Sub(t.t0).Milliseconds()
	total := ms
	t.mu.Unlock()
	t.active.Store(false)
	Infof("[ZT-COLD] first inbound data +%dms dnat=%d (total=%dms)", ms, dnat, total)
}

// expireIfStale 给长时间没等到任何证据的场景收尾（例如一直拿不到配置）。
func (t *coldStartTrace) expireIfStale(now time.Time) {
	if !t.active.Load() {
		return
	}
	t.mu.Lock()
	if t.t0.IsZero() || now.Sub(t.t0) < coldStartTraceTTL {
		t.mu.Unlock()
		return
	}
	t.mu.Unlock()
	t.active.Store(false)
	Warnf("[ZT-COLD] gave up measuring after %s (no inbound data)", coldStartTraceTTL)
}
