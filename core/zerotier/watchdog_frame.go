package zerotier

import (
	"time"
)

// frameVerdict 是数据面（ZT 业务帧）健康判定的结果。
//
// 与 judgeSocketHealth 分开：那条判据看「物理层有没有入站」，这条看
// 「送进 overlay 的帧有没有回应」。两者会分歧——换网后旧 socket 的 sendto
// 被静默丢弃，而对端 / 根服务器的 keepalive 照样收得进来，物理层看着完全
// 健康，业务帧却已经冻结（2026-11-19 实机日志：wire rx 每 10s 还在涨、
// frames rx 冻住 4 分钟）。
type frameVerdict int

const (
	frameHealthy       frameVerdict = iota // 帧在前进，或证据/时长不够
	frameRebuildSocket                     // 判死，换一个物理 socket
	frameStalled                           // 自愈手段已用尽，只能交给上层重启核心
)

// judgeFrameProgress 判定「送进了 overlay 业务帧、但一直没有回帧」的数据面僵死。
//
//	framesSent  距上次回帧以来送进 overlay 的业务帧数（只算真正的 payload，
//	            不算 keepalive —— 否则手机静置时也会被当成僵死）
//	frozenFor   距上次回帧前进的时长
//	rebuilds    本 engine 生命周期内为数据面僵死重建 socket 的次数
//	fixGap      距上次数据面重建的间隔；负数 = 从未重建过
//
// 证据不足（送出去的帧太少）一律判健康：宁可漏判，也不要因为静置手机
// 没有 overlay 流量就反复重启核心。
func judgeFrameProgress(framesSent uint64, frozenFor time.Duration, rebuilds int, fixGap time.Duration) frameVerdict {
	switch {
	case framesSent < socketFrameStallMinFrames:
		return frameHealthy
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
func (e *Engine) handleFrameStallLocked(framesRx, framesTx uint64, now time.Time) bool {
	if e.wdFrameAt.IsZero() {
		e.wdFrameAt = now
		e.wdFrameMark = framesRx
		e.wdFrameTxAt = framesTx
		return false
	}
	if framesRx > e.wdFrameMark {
		e.wdFrameAt = now
		e.wdFrameMark = framesRx
		e.wdFrameTxAt = framesTx
		e.wdFrameFrozenMs = 0
		return false
	}
	demand := framesTx - e.wdFrameTxAt
	if demand < socketFrameStallMinFrames {
		// 没往外送业务帧：帧基不前进是正常的（熄屏 / 静置 / 没在用 overlay）。
		e.wdFrameAt = now
		e.wdFrameTxAt = framesTx
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
	fixGap := time.Duration(-1)
	if !e.wdFrameFixAt.IsZero() {
		fixGap = now.Sub(e.wdFrameFixAt)
	}
	switch judgeFrameProgress(demand, frozen, e.wdFrameRebuilds, fixGap) {
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
		Warnf("[ZT-SOCKET] watchdog: %d frames sent, none came back for %s -> rebuilding socket (frame fix #%d)",
			demand, frozen.Round(time.Second), fix)
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
