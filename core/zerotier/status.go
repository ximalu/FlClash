package zerotier

import (
	"fmt"
	"sync"
	"time"
)

// RuntimeStatus is a point-in-time view of the ZeroTier runtime.
// Runtime liveness is supplied by the platform-specific implementation;
// Android/cgo reads the live Engine, while non-cgo builds report STOPPED.
//
// 未知 vs 零值（2026-11-18 评审）：凡是我们“拿不到”的数一律用 nil 指针 /
// 省略 key / "unknown" 字符串表达，不允许用 0 冒充。UI 拿到 null 就显示「—」。
type RuntimeStatus struct {
	State       string `json:"state"`
	NodeAddress string `json:"nodeAddress"`
	IPv4        string `json:"ipv4,omitempty"`
	Routes      int    `json:"routes"`
	NetworkID   string `json:"networkId,omitempty"`

	// UpdatedAt 是本快照的生成时间（unix ms）。UI 用它判断数据是否过期：
	// 超过若干个轮询周期没拿到新快照，就必须当未知处理，不能把旧读数当“现在”。
	UpdatedAt int64 `json:"updatedAt"`

	// ---- 链路层：判据只存在于 engine.go 的 watchdog（同一套常量） ----
	LinkHealth    string  `json:"linkHealth"` // ok | idle | stalled | rebuilding | unknown
	SilentForMs   *int64  `json:"silentForMs,omitempty"`
	WireTxSinceRx *uint64 `json:"wireTxSinceRx,omitempty"`
	WireRxPkts    *uint64 `json:"wireRxPkts,omitempty"`

	// ---- 物理 socket / 自愈 ----
	UDPPort          *int    `json:"udpPort,omitempty"`
	SocketGen        *uint64 `json:"socketGen,omitempty"`
	SelfHealAttempts *int    `json:"selfHealAttempts,omitempty"` // 发起重建的次数
	// SocketRebuildSuccesses = RebuildSocket 返回 nil 的次数（fd 换成）。
	// **不等于链路恢复**，强区分：恢复看 LinkRecovery*。
	SocketRebuildSuccesses *int `json:"socketRebuildSuccesses,omitempty"`
	SelfHealMax            *int `json:"selfHealMax,omitempty"`

	// 重建后的链路恢复确认（观察窗口 socketRecoveryWindow = 30s）：
	// none（从未重建）/ pending / confirmed / timeout
	LinkRecovery          string `json:"linkRecovery,omitempty"`
	LinkRecoveryConfirmed *int   `json:"linkRecoveryConfirmed,omitempty"`
	LinkRecoveryTimeout   *int   `json:"linkRecoveryTimeout,omitempty"`

	// Peers：nil = 拿不到（没有活节点 / core 返回 NULL）→ 未知；
	// 空切片 = 确实没有对端（ZT peer 是懒建立的，不是故障）。
	Peers []PeerStatus `json:"peers"`

	// PeersTruncated = 核心报的对端数超过包装层容量（64），列表被截断；
	// PeersTotal 是核心报的真实数量（未截断时为 0，看 len(Peers)）。
	PeersTruncated bool `json:"peersTruncated,omitempty"`
	PeersTotal     int  `json:"peersTotal,omitempty"`

	// LastError 只记录 StartEngine 的致命失败（见 recordStartError）。
	// 不从日志解析，也不把运行期 watchdog 的可重试失败算进来。
	LastError   string `json:"lastError,omitempty"`
	LastErrorAt int64  `json:"lastErrorAt,omitempty"`

	// Data-path diagnostics. These exist so a slow ZeroTier link can be
	// attributed to a specific stage instead of guessed at:
	//   framesDroppedC  — C ring queue full (oldest frame dropped)
	//   framesDroppedGo — Go frames channel full (newest frame dropped)
	//   wireSend*       — physical sendto() outcomes by errno class
	//   udpRcvBuf/SndBuf— buffers the kernel actually granted
	FramesDroppedC  uint64 `json:"framesDroppedC,omitempty"`
	FramesDroppedGo uint64 `json:"framesDroppedGo,omitempty"`
	WireSendOK      uint64 `json:"wireSendOk,omitempty"`
	WireSendEAGAIN  uint64 `json:"wireSendEagain,omitempty"`
	WireSendENOBUFS uint64 `json:"wireSendEnobufs,omitempty"`
	WireSendOther   uint64 `json:"wireSendOther,omitempty"`
	UDPRcvBuf       int    `json:"udpRcvBuf,omitempty"`
	UDPSndBuf       int    `json:"udpSndBuf,omitempty"`
}

// Peer path values. `relay` is defined but deliberately never produced yet:
// the C API gives us no reliable evidence of relayed vs direct (see
// PeerStatus.Path).
const (
	PeerPathDirect  = "direct"
	PeerPathRelay   = "relay"
	PeerPathUnknown = "unknown"
)

// Link health values — the single vocabulary shared by watchdog and status.
// The *judgement* lives only in engine.go (checkSocketHealth), so the Dart
// side never re-implements the thresholds.
const (
	healthUnknown    = "unknown"    // watchdog has not sampled yet (<5s after start)
	healthOK         = "ok"         // inbound seen within the sampling window
	healthIdle       = "idle"       // no inbound and no outbound demand: cannot judge
	healthStalled    = "stalled"    // outbound > 0 and silent >= socketDeadTimeout
	healthRebuilding = "rebuilding" // physical socket rebuild in flight
)

// PeerStatus is one row of the 网络 page 对端连接 list.
//
// ⚠️ Path 目前**恒为 unknown**，这是有意为之：core 的 C API 无法证明
// direct/relay。证据链（读自 ZeroTier 1.16.2 源码）：
//   - ZT_PeerPhysicalPath.trustedPathId 来自**本地** trusted-path 配置
//     （Node.cpp: getOutboundPathTrust(address)），它不是 relay 标记。
//   - core 自己的 CLI 只有在 TCP fallback 隧道建立、或“没有 preferred
//     路径”时才打印 RELAY（one.cpp）。
//   - preferred 只要求路径年轻于 ZT_PEER_PATH_EXPIRATION，不要求它真的
//     在收包——2026-11-17 现场 i3 侧显示 DIRECT 但 ping 0/20 就是反例。
//   - 中继场景下对端的路径地址其实是中继的地址，lastReceive 也会很新，
//     用收包时间反推同样会误判成 direct。
// 所以这一列宁可显示「—」。PreferredPaths / PathsWithRx / LastReceiveAtMs
// 是原始证据，只放到 RPC 里供以后（或诊断）用，不参与 UI 判定。
type PeerStatus struct {
	Address         string `json:"address"`
	LatencyMs       int    `json:"latencyMs"` // -1 = core 没有测到
	Version         string `json:"version,omitempty"`
	Role            string `json:"role,omitempty"`
	Path            string `json:"path"`
	PathCount       int    `json:"pathCount"`
	PreferredPaths  int    `json:"preferredPaths,omitempty"`
	PathsWithRx     int    `json:"pathsWithRx,omitempty"`
	LastReceiveAtMs int64  `json:"lastReceiveAtMs,omitempty"`
	LastSendAtMs    int64  `json:"lastSendAtMs,omitempty"`
}

// ---------------------------------------------------------------------------
// Last fatal start error
//
// 生命周期规则（2026-11-18 评审要求写清楚）：
//   来源：只来自 StartEngine 的失败（bind / node / join 等）
//   时机：StartEngine 返回错误时写入
//   清除：StartEngine 成功、以及 Stop 完成时清空
// 用包级变量而不是 Engine 字段：启动失败的 Engine 会被 abort 丢弃，
// 但 UI 仍需要在“没有引擎”的状态下看到失败原因。
// ---------------------------------------------------------------------------
var (
	startErrMu sync.Mutex
	startErr   string
	startErrAt time.Time
)

func recordStartError(err error) {
	if err == nil {
		return
	}
	startErrMu.Lock()
	startErr = err.Error()
	startErrAt = time.Now()
	startErrMu.Unlock()
}

func clearStartError() {
	startErrMu.Lock()
	startErr = ""
	startErrAt = time.Time{}
	startErrMu.Unlock()
}

func currentStartError() (string, int64) {
	startErrMu.Lock()
	defer startErrMu.Unlock()
	if startErr == "" {
		return "", 0
	}
	return startErr, startErrAt.UnixMilli()
}

// ---------------------------------------------------------------------------
// Small formatters shared by the status paths (pure, testable without cgo).
// ---------------------------------------------------------------------------

func formatPeerVersion(major, minor, rev int) string {
	if major < 0 {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d", major, minor, rev)
}

func peerRoleString(role int32) string {
	switch role {
	case 0:
		return "LEAF"
	case 1:
		return "MOON"
	case 2:
		return "PLANET"
	}
	return ""
}
