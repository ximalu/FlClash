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
//
// 三条处置档位（按代价从小到大，只升不降）：
//
//	frameRebuildSocket  换一个物理 socket（快、无状态损失）
//	frameRestartNode    重建整个 ZT Node（新 node / 新 socket / 重新 join）
//	frameStalled        自愈手段用尽，交给上层重启核心
type frameVerdict int

const (
	frameHealthy       frameVerdict = iota // 帧在前进，或证据/时长不够
	frameRebuildSocket                     // 判死，换一个物理 socket
	frameRestartNode                       // 换 socket 无效，重建整个 ZT Node
	frameStalled                           // 自愈手段已用尽，只能交给上层重启核心
)

// judgeFrameProgress 判定「送进了 overlay 业务帧、但一直没有回帧」的数据面僵死。
//
//	framesSent   距上次回帧以来送进 overlay 的业务帧数（只算真正的 payload，
//	             不算 keepalive —— 否则手机静置时也会被当成僵死）
//	frozenFor    距上次回帧前进的时长
//	rebuilds     本 engine 生命周期内为数据面僵死重建 socket 的次数
//	fixGap       距上次数据面 socket 重建的间隔；负数 = 从未重建过
//	nodeRestarts 本 engine 生命周期内重建 ZT Node 的次数
//	nodeGap      距上次重建 Node 的间隔；负数 = 从未重建过
//
// 证据不足（送出去的帧太少）一律判健康：宁可漏判，也不要因为静置手机
// 没有 overlay 流量就反复重启核心。
//
// 升级顺序：先换 socket（最多 socketFrameStallMax 次、每次之间
// socketFrameStallMinGap），换不动了才重建 node（最多
// socketFrameNodeRestartMax 次、间隔 socketFrameNodeRestartMinGap），
// node 也用尽才判 stalled。2026-11-19 实机证明「只换 socket」救不回来，
// 所以 node 这一档必须有，且必须是自动的（用户手动杀进程才管用不算修好）。
func judgeFrameProgress(
	framesSent uint64,
	frozenFor time.Duration,
	rebuilds int,
	fixGap time.Duration,
	nodeRestarts int,
	nodeGap time.Duration,
) frameVerdict {
	switch {
	case framesSent < socketFrameStallMinFrames:
		return frameHealthy
	case frozenFor < socketFrameStallTimeout:
		return frameHealthy
	case rebuilds < socketFrameStallMax && (fixGap < 0 || fixGap >= socketFrameStallMinGap):
		return frameRebuildSocket
	// 换 socket 这一档暂时不可用（用尽次数，或刚换过还在观察窗内）：
	// 只有冻得够久才升级重建 node，免得一个 20s 的抖动就把 node 重建掉。
	case nodeRestarts >= socketFrameNodeRestartMax:
		return frameStalled
	case frozenFor < socketFrameNodeRestartFrozenFor:
		return frameHealthy
	case nodeGap >= 0 && nodeGap < socketFrameNodeRestartMinGap:
		return frameHealthy // node 刚重建过，观察窗内
	default:
		return frameRestartNode
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
		// 还没送出足够多的业务帧：不能判死，但**也不能把需求基线拉前**。
		// 2026-11-25 实机：换网后应用只零星重试（每 10s 一两个 SYN），
		// 每个 tick 的 demand 都 < minFrames，旧代码于是每 5s 重置一次，
		// frozen 永远长不起来 → 整场 3 分钟僵死期间 node 重建档位一次都没触发。
		// 现在只把冻结时钟跟随到 now（等于“还没开始冻结”），需求基线保持不动，
		// 等累计出站达到门槛后再开始计时。
		e.wdFrameAt = now
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
	nodeGap := time.Duration(-1)
	if !e.wdFrameNodeAt.IsZero() {
		nodeGap = now.Sub(e.wdFrameNodeAt)
	}
	switch judgeFrameProgress(demand, frozen, e.wdFrameRebuilds, fixGap, e.wdNodeRestarts, nodeGap) {
	case frameStalled:
		e.wdHealth = healthStalled
		e.wdMu.Unlock()
		return true
	case frameRestartNode:
		e.wdFrameStalls++
		e.wdNodeRestarts++
		restarts := e.wdNodeRestarts
		rebuilds := e.wdFrameRebuilds
		e.wdFrameNodeAt = now
		e.wdHealth = healthRebuilding
		e.wdMu.Unlock()
		Warnf("[ZT] watchdog: %d frames sent, none came back for %s after %d socket rebuilds -> restarting ZeroTier node (node restart #%d)",
			demand, frozen.Round(time.Second), rebuilds, restarts)
		// ⚠️ 2026-11-24 实机死锁（1904 上 ZT 永久不恢复的真因）：这里原本**同步**
		// 调 RestartNode()，而 RestartNode → teardown() → wg.Wait() 会等待
		// socketWatchdog 自己（它是 start() 里 wg.Add(4) 的四个 goroutine 之一）
		// → 永久自锁：socket 已关、node 没删、引擎卡在 StateStopping；日志里只
		// 剩下 "restarting ZeroTier node" 一行，连失败行都没有，之后 wire rx 全冻。
		// 必须放到**不进 wg 的** goroutine 里跑。
		go func() {
			defer func() {
				if r := recover(); r != nil {
					Warnf("[ZT] watchdog node restart panicked: %v", r)
				}
			}()
			if err := e.RestartNode("watchdog: data plane stalled after socket rebuilds"); err != nil {
				Warnf("[ZT] watchdog node restart failed: %v", err)
				e.setHealth(healthStalled)
				e.DiagSnapshot("node-restart-failed")
				return
			}
			// 新 node + 新 socket：帧基线与 socket 档位计数一起归零，给这次重建
			// 一个完整观察窗（否则会用旧 socket 的观察窗立刻再判死）。
			e.wdMu.Lock()
			e.wdFrameAt = time.Now()
			e.wdFrameMark = e.ztFramesRx.Load()
			e.wdFrameTxAt = e.ztFramesTx.Load()
			e.wdFrameRebuilds = 0
			e.wdFrameFixAt = time.Time{}
			// 重建后链路恢复确认重新开始：这次换的是 node，不是 socket。
			e.wdRecoveryGen = e.socketGen.Load()
			e.wdRecoveryRxBase = e.wireRxPkts.Load()
			e.wdRecoveryUntil = time.Now().Add(socketRecoveryWindow)
			e.wdRecoveryDone = false
			e.wdRecoveryState = "pending"
			e.wdMu.Unlock()
			e.DiagSnapshot("node-restart-ok")
		}()
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
