package zerotier

import (
	"time"
)

// frameVerdict 是数据面（ZT 帧）健康判定的结果。
//
// 与 judgeSocketHealth 分开：那条判据看的是「物理层有没有入站」，这条看的是
// 「业务帧有没有前进」。两者会分歧——换网后旧 socket 的 sendto 被静默丢弃，
// 但对端 / 根服务器的 keepalive 照样收得进来，物理层看着完全健康，而业务帧
// 已经冻结（2026-11-19 实机日志：wire rx 每 10s 还在涨、frames rx 冻住）。
type frameVerdict int

const (
	frameHealthy       frameVerdict = iota // 帧在前进，或还没到判死时长
	frameIdle                              // 没有出站需求：熄屏 / 无流量，不能判死
	frameRebuildSocket                     // 判死，换一个物理 socket
	frameStalled                           // 自愈手段已用尽，只能交给上层重启核心
)

// judgeFrameProgress 判定「物理层还在收包、业务帧零进展」的数据面僵死。
//
//	sampled    watchdog 是否已经采过样
//	demand     距上次帧前进以来「想发出去」的包数（成功 + 各类 errno）
//	frozenFor  距上次帧前进的时长
//	rebuilds   本 engine 生命周期内为数据面僵死重建 socket 的次数
//	fixGap     距上次数据面重建的间隔；负数 = 从未重建过
//
// 返回 frameRebuildSocket 表示此刻允许重建；frameStalled 表示重建次数已用尽，
// 这时换 socket 已经证明没用，必须靠上层把核心整个重启（watchdog 之外的动作）。
func judgeFrameProgress(sampled bool, demand uint64, frozenFor time.Duration, rebuilds int, fixGap time.Duration) frameVerdict {
	switch {
	case !sampled:
		return frameHealthy
	case demand == 0:
		// 没有出站需求（熄屏 / 静置）：帧冻着很正常，不能判死。
		return frameIdle
	case frozenFor < socketFrameStallTimeout:
		return frameHealthy
	case rebuilds >= socketFrameStallMax:
		return frameStalled
	case fixGap >= 0 && fixGap < socketFrameStallMinGap:
		return frameHealthy // 刚重建过，给新 socket 一个观察窗
	default:
		return frameRebuildSocket
	}
}

// handleFrameStallLocked 判定并处理数据面僵死。
//
// 调用方必须持有 wdMu。返回 true = 已处理（内部已解锁，调用方必须立即返回）；
// 返回 false = 没处理，锁仍由调用方持有。
func (e *Engine) handleFrameStallLocked(frames, txDemand uint64, now time.Time) bool {
	if e.wdFrameAt.IsZero() {
		e.wdFrameAt = now
		e.wdFrameMark = frames
		e.wdFrameTxAt = txDemand
		return false
	}
	if frames > e.wdFrameMark {
		e.wdFrameAt = now
		e.wdFrameMark = frames
		e.wdFrameTxAt = txDemand
		e.wdFrameFrozenMs = 0
		return false
	}
	frozen := now.Sub(e.wdFrameAt)
	frozenMs := frozen.Milliseconds()
	if frozenMs < 0 {
		frozenMs = 0
	}
	// 冻结时长不因重建 socket 而清零：上层用它判断「换 socket 之后还是不动」，
	// 从而决定要不要把核心整个重启一次。
	e.wdFrameFrozenMs = frozenMs
	demand := txDemand - e.wdFrameTxAt
	fixGap := time.Duration(-1)
	if !e.wdFrameFixAt.IsZero() {
		fixGap = now.Sub(e.wdFrameFixAt)
	}
	switch judgeFrameProgress(true, demand, frozen, e.wdFrameRebuilds, fixGap) {
	case frameIdle:
		e.wdFrameAt = now
		e.wdFrameFrozenMs = 0
		return false
	case frameStalled:
		e.wdHealth = healthStalled
		e.wdMu.Unlock()
		return true
	case frameRebuildSocket:
		e.wdFrameStalls++
		e.wdFrameRebuilds++
		fix := e.wdFrameRebuilds
		e.wdFrameFixAt = now
		e.wdHealth = healthRebuilding
		e.wdMu.Unlock()
		Warnf("[ZT-SOCKET] watchdog: no ZT frame for %s while %d outbound packets were sent -> rebuilding socket (frame fix #%d)",
			frozen.Round(time.Second), demand, fix)
		if err := e.RebuildSocket("watchdog: data plane stalled"); err != nil {
			Warnf("[ZT-SOCKET] watchdog frame rebuild failed: %v", err)
			e.setHealth(healthStalled)
			return true
		}
		e.wdMu.Lock()
		e.wdFixOK++
		e.wdRecoveryGen = e.socketGen.Load()
		e.wdRecoveryRxBase = e.wireRxPkts.Load()
		e.wdRecoveryUntil = time.Now().Add(socketRecoveryWindow)
		e.wdRecoveryDone = false
		e.wdRecoveryState = "pending"
		e.wdMu.Unlock()
		return true
	}
	return false
}
