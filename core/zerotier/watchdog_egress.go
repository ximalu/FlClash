//go:build cgo

package zerotier

/*
#include "wrapper.h"
*/
import "C"

import (
	"strconv"
	"time"
)

// 出向路径失效（one-way path failure）
//
// 2026-11-26 深夜抓包实证：手机在 WiFi 上静止时，i3→手机 正常（手机侧 peer
// lastRx=6s、lat=41ms），手机→i3 的包 i3 一个都没收到；手机侧 snat 一直在涨、
// dnat 冻结 5 分钟，而两条现有 watchdown 都判「健康」：
//
//	物理层判据：wire rx 在涨（其它 peer 的 keepalive 照样进来）
//	数据面判据：frames rx 在涨（同上，+50 个帧）
//
// 所以需要第三条判据，只看「我们自己发出去的包有没有人回来」——adapter 的
// snat/dnat 是这条链路上唯一诚实的信号：dnat = peer→TUN 的包，业务真的回来了
// 它才会动。判死后的处置也比换 socket 更精确：只把出问题的 LEAF peer 的路径
// 重置一次（全局硬 reset 会连健康 peer 的端点记忆一起丢掉）。

type egressVerdict int

const (
	egressHealthy    egressVerdict = iota // 回帧在前进，或证据不够
	egressResetPaths                      // 判死：只对 LEAF peer 做一次路径重置
	egressStalled                         // 复位次数用尽：交给上层阶梯，不再动
)

var (
	// 判死门槛：至少送出这么多帧、且 dnat 冻结这么久。间隔是 5 s 采样，
	// 20 s = 4 个采样点，足够过滤「刚重连时 TCP 在重试」的正常抖动。
	egressStallMinFrames = uint64(8)
	egressStallTimeout   = 20 * time.Second
	// 每个 socket 世代内最多复位这么多次；两次之间至少间隔 30 s（给对端
	// 重新学习端点的时间，否则会把刚学到的路径又清掉）。
	egressResetMax    = 2
	egressResetMinGap = 30 * time.Second
)

// judgeEgressProgress 判定「一直在发、但一个回帧都没有」。
//
//	snatSince  距 dnat 上次前进以来，TUN→ZT 成功发出的 IP 包数（有出站需求）
//	dnatSince  同期从 ZT 回到 TUN 的 IP 包数（正常应 > 0）
//	frozenFor  dnat 冻结了多久
//	resets     本 socket 世代内已做的 peer 级复位次数
//	resetGap   距上次复位的间隔；负数 = 从未复位过
//
// 证据不足一律判健康：静置的手机没有出站需求时，绝不能因为「没回帧」就复位。
func judgeEgressProgress(
	snatSince uint64,
	dnatSince uint64,
	frozenFor time.Duration,
	resets int,
	resetGap time.Duration,
) egressVerdict {
	switch {
	case dnatSince > 0:
		return egressHealthy
	case snatSince < egressStallMinFrames:
		return egressHealthy
	case frozenFor < egressStallTimeout:
		return egressHealthy
	case resets >= egressResetMax:
		return egressStalled
	case resetGap >= 0 && resetGap < egressResetMinGap:
		return egressHealthy // 刚复位过，还在观察窗内
	default:
		return egressResetPaths
	}
}

// handleEgressStallLocked 每个 socket 采样周期调一次。调用方必须持有 wdMu。
// 返回 true = 已处理（内部已解锁，调用方必须立即返回）。
func (e *Engine) handleEgressStallLocked(now time.Time) bool {
	if e.egressStats == nil {
		return false
	}
	snat, dnat := e.egressStats()
	gen := e.socketGen.Load()

	// 换过 socket / 刚重建过 node：路径重新学，基线必须重来。
	if gen != e.wdEgressGen || e.wdEgressAt.IsZero() {
		e.wdEgressGen = gen
		e.wdEgressAt = now
		e.wdEgressSnat = snat
		e.wdEgressDnat = dnat
		e.wdEgressResets = 0
		e.wdEgressFrozenMs = 0
		return false
	}
	if dnat > e.wdEgressDnat {
		// 回帧了：这就是「链路真的活着」的唯一正面证据。
		e.wdEgressAt = now
		e.wdEgressSnat = snat
		e.wdEgressDnat = dnat
		e.wdEgressFrozenMs = 0
		return false
	}

	frozen := now.Sub(e.wdEgressAt)
	frozenMs := frozen.Milliseconds()
	if frozenMs < 0 {
		frozenMs = 0
	}
	e.wdEgressFrozenMs = frozenMs
	snatSince := snat - e.wdEgressSnat
	resetGap := time.Duration(-1)
	if !e.wdEgressResetAt.IsZero() {
		resetGap = now.Sub(e.wdEgressResetAt)
	}

	switch judgeEgressProgress(snatSince, 0, frozen, e.wdEgressResets, resetGap) {
	case egressHealthy:
		return false
	case egressStalled:
		// 复位次数用尽：链路仍然单向。每 60 s 才告警一次，避免 5 s 一个 tick 涮屏。
		if now.Sub(e.wdEgressWarnAt) >= 60*time.Second {
			e.wdEgressWarnAt = now
			Warnf("[ZT-EGRESS] still one-way after %d peer resets: %d frames sent, dnat frozen %s (escalation left to the ladder)",
				e.wdEgressResets, snatSince, frozen.Round(time.Second))
		}
		return false
	default:
		e.wdEgressResets++
		e.wdEgressResetAt = now
		// 复位后给对端一个完整的观察窗，否则下一轮采样会拿旧冻结时长再判一次。
		e.wdEgressAt = now
		e.wdEgressSnat = snat
		resets := e.wdEgressResets
		e.wdMu.Unlock()
		Warnf("[ZT-EGRESS] outbound path dead: %d frames sent, 0 came back for %s -> resetting LEAF peer paths (#%d)",
			snatSince, frozen.Round(time.Second), resets)
		go e.recoverEgressPaths(snatSince, frozen)
		return true
	}
}

// recoverEgressPaths 只重置「入向还活着」的 LEAF peer：这正是单向断链的特征 ——
// 我们发出去的帧没有回帧（dnat 冻结），而对方的 keepalive 还在进来（lastRx 新鲜），
// 说明死的是**出向**那半条路。若一个这样的 peer 都没有（对方也全哑了），那属于双向
// 断链，该由 socket 看门狗处理，这里不动 —— 否则静置手机也会被乱复位。
//
// 只动 LEAF；root/planet 的路径是 relay 的生命线，绝不能被我们清掉。
func (e *Engine) recoverEgressPaths(snatSince uint64, frozen time.Duration) {
	snap := e.PeerSnapshot()
	if !snap.Known {
		Warnf("[ZT-EGRESS] peer snapshot unavailable, cannot reset paths")
		return
	}
	targets := egressTargetPeers(snap.Peers, time.Now().UnixMilli())
	if len(targets) == 0 {
		Warnf("[ZT-EGRESS] no inbound-alive LEAF peer to reset (both directions silent); leaving it to the socket watchdog")
		return
	}
	reset := 0
	for _, p := range targets {
		addr, err := strconv.ParseUint(p.Address, 16, 64)
		if err != nil || addr == 0 {
			continue
		}
		if rc := int(C.flclashtier_zt_reset_peer_path(C.uint64_t(addr), 0)); rc == 0 {
			reset++
		}
	}
	helloed := int(C.flclashtier_zt_rehello_peers())
	Infof("[ZT-EGRESS] peer-path reset done: reset=%d/%d relayedHELLO=%d (snat %d, frozen %s)",
		reset, len(snap.Peers), helloed, snatSince, frozen.Round(time.Second))
	e.logPeerPaths("after-egress-reset")
}

// egressResetPeerFreshWindow：对端「还在给我们发东西」的判定窗口。
const egressResetPeerFreshWindow = 15 * time.Second

// egressTargetPeers 挑出该复位的 peer：LEAF、有路径、且最近 15 s 内收到过它的包。
// 纯函数，便于单测。
func egressTargetPeers(peers []PeerStatus, nowMs int64) []PeerStatus {
	out := make([]PeerStatus, 0, len(peers))
	fresh := egressResetPeerFreshWindow.Milliseconds()
	for _, p := range peers {
		if p.Role != "LEAF" || p.PathCount == 0 || p.LastReceiveAtMs <= 0 {
			continue
		}
		if nowMs-p.LastReceiveAtMs > fresh {
			continue
		}
		out = append(out, p)
	}
	return out
}

// SetEgressStatsProvider 注入数据面 snat/dnat 计数器（由 tun 层的 adapter 提供）。
// nil 表示没有数据面（纯 ZT 测试或启动早期），出向判据自动停用。
func (e *Engine) SetEgressStatsProvider(f func() (snat, dnat uint64)) {
	e.wdMu.Lock()
	e.egressStats = f
	e.wdEgressGen = 0
	e.wdEgressAt = time.Time{}
	e.wdMu.Unlock()
}

// resetWatchdogBaselines 把两条看门狗的基线一起归零。任何 node 重建之后都必须调：
// 新 node 的帧/数据面计数与新 socket 一起从零开始，沿用旧基线只会在十几秒后误判一次
// （2026-11-27：阶梯重启后 15 s 帧看门狗又重启了一次）。
func (e *Engine) resetWatchdogBaselines(now time.Time) {
	e.wdMu.Lock()
	defer e.wdMu.Unlock()
	e.wdFrameAt = now
	e.wdFrameMark = e.ztFramesRx.Load()
	e.wdFrameTxAt = e.ztFramesTx.Load()
	e.wdFrameFrozenMs = 0
	e.wdFrameRebuilds = 0
	e.wdFrameFixAt = time.Time{}
	e.wdEgressGen = e.socketGen.Load()
	e.wdEgressAt = now
	e.wdEgressResets = 0
	e.wdEgressFrozenMs = 0
	if e.egressStats != nil {
		e.wdEgressSnat, e.wdEgressDnat = e.egressStats()
	}
}
