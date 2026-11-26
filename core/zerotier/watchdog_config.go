//go:build cgo

package zerotier

/*
#include "wrapper.h"
*/
import "C"

import "time"

// 配置卡死看门狗（2026-11-27 真机：装完 0.9.26 后开机 3.5 分钟仍是
// "尚未分配到 IP"，st=0 rev=0，而 wire tx 一直在涨）。
//
// 机制：controller 的节点地址就是 network id 的高 40 位（ZeroTier 规定），所以
// 「把配置请求发给谁」是已知的 —— 我们可以只对**那一个 peer** 做路径复位，逼两端
// 重新学端点。观察到的现场特征：i3 上 controller(b6079f73c6) 挂着 15 条陈旧路径、
// latency=-1，而 VPS 上同一 controller 是 177 ms 正常 —— 典型的「我们记着旧端点，
// 配置答复回不来」。这种情况下 ZT 自己只会在 ZT_PEER_PATH_EXPIRATION(243 s) 之后
// 才慢慢换路，而客户端只能干等「尚未分配到 IP」。
//
// 判据必须带 wire tx 增量：链路本身没发包（比如 socket 还没绑定）时不要动手。

const (
	configStallTimeout   = 45 * time.Second
	configStallMinWireTx = 20
	configStallMaxNudges = 3
	configStallNudgeGap  = 60 * time.Second
	configStallWarnGap   = 5 * time.Minute
	// 复位也救不回时，用「冷启动」兜底（重建整个 node = 重新 join + 重新拉配置）：
	// 2026-11-27 实测 Dev 0.9.1 冷启动 20 s 就能拿到配置，而卡住的 node 3.5 分钟
	// 一个配置包都没收到。2 分钟一次，避免反复 teardown。
	configStallRestartGap = 2 * time.Minute
)

type configVerdict int

const (
	configHealthy   configVerdict = iota
	configNudge                   // 对 controller 做一次路径复位 + relayed HELLO
	configExhausted               // 复位次数用尽：只告警
)

// judgeConfigStall 判定「还在等网络配置」是否已经异常。
//
//	status        ZT_VirtualNetworkStatus 当前值（1 = OK）
//	stalledFor    从第一次看到非 OK 起持续多久
//	wireTxDelta   同期 wire 发送成功增量（没在发 = 证据不足，不动）
//	nudges        本 node 世代内已做过几次复位
//	nudgeGap      距上次复位的间隔；负数 = 从未复位
func judgeConfigStall(status int, stalledFor time.Duration, wireTxDelta uint64, nudges int, nudgeGap time.Duration) configVerdict {
	switch {
	case status == statusOK:
		return configHealthy
	case stalledFor < configStallTimeout:
		return configHealthy
	case wireTxDelta < configStallMinWireTx:
		return configHealthy
	case nudges >= configStallMaxNudges:
		return configExhausted
	case nudgeGap >= 0 && nudgeGap < configStallNudgeGap:
		return configHealthy
	default:
		return configNudge
	}
}

// handleConfigStallLocked 每个 socket 采样周期调一次。调用方必须持有 wdMu。
// 返回 true = 已处理（内部已解锁，调用方必须立即返回）。
func (e *Engine) handleConfigStallLocked(now time.Time) bool {
	if e.restarting.Load() || e.rebuilding.Load() {
		return false
	}
	snap, ok := e.Current()
	if !ok {
		return false
	}
	if snap.Status == statusOK {
		e.wdConfigAt = time.Time{}
		e.wdConfigNudges = 0
		e.wdConfigFrozenMs = 0
		return false
	}

	_, txOK, _, _, _ := e.DataPathCounters()
	if e.wdConfigAt.IsZero() {
		e.wdConfigAt = now
		e.wdConfigTxMark = txOK
		return false
	}
	frozen := now.Sub(e.wdConfigAt)
	e.wdConfigFrozenMs = frozen.Milliseconds()
	gap := time.Duration(-1)
	if !e.wdConfigNudgeAt.IsZero() {
		gap = now.Sub(e.wdConfigNudgeAt)
	}
	delta := uint64(0)
	if txOK > e.wdConfigTxMark {
		delta = txOK - e.wdConfigTxMark
	}

	switch judgeConfigStall(snap.Status, frozen, delta, e.wdConfigNudges, gap) {
	case configHealthy:
		return false
	case configExhausted:
		// 复位次数用尽：改成冷启动兜底（每 2 分钟最多一次），否则只告警。
		if now.Sub(e.wdConfigRestartAt) >= configStallRestartGap {
			e.wdConfigRestartAt = now
			e.wdConfigNudges = 0
			e.wdConfigAt = now
			e.wdConfigTxMark = txOK
			e.wdMu.Unlock()
			Warnf("[ZT-CONF] controller path reset did not help (%s, st=%d rev=%d) -> cold restart of the ZT node",
				frozen.Round(time.Second), snap.Status, e.ConfigRevision())
			go func() {
				if err := e.RestartNode("config stalled"); err != nil {
					Warnf("[ZT-CONF] cold restart failed: %v", err)
				}
			}()
			return true
		}
		if now.Sub(e.wdConfigWarnAt) >= configStallWarnGap {
			e.wdConfigWarnAt = now
			Warnf("[ZT-CONF] still no config after %s and %d controller-path resets (st=%d rev=%d wireTx +%d) — controller unreachable or member unauthorized",
				frozen.Round(time.Second), e.wdConfigNudges, snap.Status, e.ConfigRevision(), delta)
		}
		return false
	default:
		e.wdConfigNudges++
		e.wdConfigNudgeAt = now
		// 复位后给一个新的观察窗口（否则同一个冻结时长会立刻再判一次）。
		e.wdConfigAt = now
		e.wdConfigTxMark = txOK
		nudges := e.wdConfigNudges
		ctrl := snap.Nwid >> 24
		e.wdMu.Unlock()
		Warnf("[ZT-CONF] no config for %s (st=%d rev=%d wireTx +%d) -> resetting controller path %010x (#%d)",
			frozen.Round(time.Second), snap.Status, e.ConfigRevision(), delta, ctrl, nudges)
		go e.nudgeControllerPaths(ctrl, frozen, delta)
		return true
	}
}

// nudgeControllerPaths 只复位 controller 那一个 peer 的路径，然后经 root 补一次
// relayed HELLO。不动其它 peer：其它 peer 的路径是业务链路，跟配置下发无关。
func (e *Engine) nudgeControllerPaths(ctrl uint64, frozen time.Duration, delta uint64) {
	if ctrl == 0 {
		return
	}
	rc := int(C.flclashtier_zt_reset_peer_path(C.uint64_t(ctrl), 0))
	helloed := int(C.flclashtier_zt_rehello_peers())
	Infof("[ZT-CONF] controller path reset: addr=%010x rc=%d relayedHELLO=%d (stalled %s, wireTx +%d)",
		ctrl, rc, helloed, frozen.Round(time.Second), delta)
	e.logPeerPaths("after-config-stall")
}
