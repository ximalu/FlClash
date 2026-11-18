//go:build cgo

package zerotier

import "time"

// GetRuntimeStatus returns the current ZeroTier runtime state from the live
// in-memory Engine. No persisted file or heartbeat participates in liveness.
//
// 未知语义（2026-11-18 评审）：
//   - 没有引擎 → LinkHealth=unknown、Peers=nil、socket/自愈字段全缺（不是 0）
//   - 引擎在跑但 watchdog 还没采样 → 同样 unknown
//   - LastError 来自包级 startErr（启动失败的 Engine 已被 abort，拿不到它）
func GetRuntimeStatus() RuntimeStatus {
	now := time.Now()
	status := RuntimeStatus{
		State:      StateStopped.String(),
		UpdatedAt:  now.UnixMilli(),
		LinkHealth: healthUnknown,
		Peers:      nil,
	}
	if msg, at := currentStartError(); msg != "" {
		status.LastError = msg
		status.LastErrorAt = at
	}

	e := currentEngine()
	if e == nil {
		return status
	}

	status.State = e.getState().String()
	status.NodeAddress = formatNodeAddress(e.nodeAddress())
	status.NetworkID = e.cfg.NetworkID

	e.fillDataPathStatus(&status)

	if snap, ok := e.Current(); ok {
		status.Routes = len(snap.Routes)
		if addr := firstAssigned4(snap.Assigned); addr.IsValid() {
			status.IPv4 = addr.String()
		}
	}

	if port := e.boundPort(); port > 0 {
		status.UDPPort = &port
	}
	gen := e.SocketGen()
	status.SocketGen = &gen

	attempts, successes, max := e.selfHealStats()
	status.SelfHealAttempts = &attempts
	status.SelfHealSuccesses = &successes
	status.SelfHealMax = &max

	link := e.linkStats(now)
	status.LinkHealth = link.Health
	status.SilentForMs = link.SilentMs
	status.WireTxSinceRx = link.WireTxSinceRx
	rxPkts := link.WireRxPkts
	status.WireRxPkts = &rxPkts

	// ok=false（没有活节点 / core 返回 NULL）时保持 nil = 未知；
	// 空切片（非 nil）才是“确实没有对端”。
	if peers, ok := e.PeerStatuses(); ok {
		status.Peers = peers
	}
	return status
}

// fillDataPathStatus copies the live data-path counters into the status.
func (e *Engine) fillDataPathStatus(st *RuntimeStatus) {
	fd, ok, ea, en, ot := e.DataPathCounters()
	st.FramesDroppedC = fd
	st.WireSendOK = ok
	st.WireSendEAGAIN = ea
	st.WireSendENOBUFS = en
	st.WireSendOther = ot
	st.FramesDroppedGo = e.FramesDropped()
	st.UDPRcvBuf, st.UDPSndBuf = UDPSocketBuffers()
}

func formatNodeAddress(address uint64) string {
	if address == 0 {
		return ""
	}
	const hex = "0123456789abcdef"
	var buf [10]byte
	for i := len(buf) - 1; i >= 0; i-- {
		buf[i] = hex[address&0xf]
		address >>= 4
	}
	return string(buf[:])
}
