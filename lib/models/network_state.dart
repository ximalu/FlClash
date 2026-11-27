/// FlClashTier 网络状态模型（v1 原型）
///
/// 设计文档：kb/projects/flclashtier/network-state-model.md
///
/// 原则 P4：UI 只消费 NetworkState，不直接读 core。接真数据时只替换
/// providers/network.dart 里的构造逻辑，本文件与视图层不需要改。
library;

import 'dart:io';

/// 服务层：系统 VPN 通道。
enum VpnStatus { off, connecting, on, unknown }

/// 运行层：单个出口的实际运行状态。
enum ExitRunStatus { notEnabled, stopped, starting, running, error }

/// 链路层：ZT 物理通道健康度（判据来自 engine.go 的 watchdog 常量）。
///
/// 语义边界（本轮评审明确）：
/// - [ok] 仅表示**观察到物理 UDP 入站**，不代表 peer 可用、更不代表业务可用；
/// - [idle] 只表示缺乏通信证据（没有出站需求，如熄屏/无流量），**不是健康保证**；
/// - [unknown] 还没有判据（引擎刚起 / watchdog 未采样 / 数据过期 / 采样被系统
///   挂起后重新开始）；
/// - 不会为了消除 idle 而制造后台流量。
enum LinkHealth { ok, idle, stalled, rebuilding, unknown }

/// 派生 UI 状态（network-state-model.md §5，按优先级匹配）。
enum NetworkUiState {
  s0VpnOff,
  s1Normal,
  s2LinkStalled,
  s3WaitingConfig,
  s4NoProbe,
  s5OnlyProxy,
  s6StartFailed,
  s7Starting,
}

/// mihomo 侧置灰原因（本期不支持，见设计文档 §7）。
const String kMihomoLifecycleNote = '需要底层内核生命周期改造';

/// 一个网络出口（意图 + 运行 + 展示行）。
class ExitState {
  final String name;
  final String subtitle;

  /// 用户启用意图（持久化偏好），不是运行状态。
  final bool enabled;

  final ExitRunStatus status;

  /// 卡片正文附加行（最多 2 行，多余由视图省略）。
  final List<String> lines;

  /// 非空 = 开关置灰，值即置灰理由文案。
  final String? switchDisabledReason;

  /// 非空 = 卡片里额外显示的一行实时速率（目前只有 ZT 卡片用）。
  final String? trafficLine;

  const ExitState({
    required this.name,
    required this.subtitle,
    required this.enabled,
    required this.status,
    this.lines = const [],
    this.switchDisabledReason,
    this.trafficLine,
  });
}

/// 对端（纯 core 事实：40bit address + 延迟 + 路径计数）。
class PeerInfo {
  final String address;
  final int? latencyMs;

  /// core 报的「被选中路径数」（preferredPaths）。
  ///
  /// 这是**可验证**的计数；direct/relay 从 C API 拿不到可靠证据
  /// （见 core/zerotier/status.go 的 PeerStatus 注释），所以界面只展示
  /// 这个数字，不再写“直连/中继/连接方式未知”。
  final int preferredPaths;
  final String version;

  /// core 报的角色：`LEAF` / `MOON` / `PLANET`（空 = 未知）。
  /// 摘要只把 LEAF 当成“真实设备”，PLANET/MOON 是基础设施。
  final String role;

  /// 最近一次从该对端收到包的时间（unix ms，null/0 = 未知/从未）。
  final int? lastReceiveAtMs;

  const PeerInfo({
    required this.address,
    this.latencyMs,
    this.preferredPaths = 0,
    this.version = '',
    this.role = '',
    this.lastReceiveAtMs,
  });
}

/// 探测目标的健康档位（带迟滞，见 [deriveProbeHealth]）。
enum ProbeHealth { unknown, ok, degraded, down }

/// 第一版只接受 IP 字面量（不做 DNS 解析）：
/// 否则“探测到的是哪个出口 / 哪次解析结果”无法说清。
bool isValidProbeHost(String host) =>
    host.trim().isNotEmpty && InternetAddress.tryParse(host.trim()) != null;

/// 探测目标（持久化配置，name 由用户填写）。
class ProbeTarget {
  final String id;
  final String name;
  final String host;
  final int port;
  final bool enabled;

  const ProbeTarget({
    required this.id,
    required this.name,
    required this.host,
    required this.port,
    this.enabled = true,
  });

  String get endpoint => '$host:$port';

  bool get isValid => isValidProbeHost(host) && port > 0 && port <= 65535;

  ProbeTarget copyWith({String? name, String? host, int? port, bool? enabled}) {
    return ProbeTarget(
      id: id,
      name: name ?? this.name,
      host: host ?? this.host,
      port: port ?? this.port,
      enabled: enabled ?? this.enabled,
    );
  }

  Map<String, dynamic> toJson() => {
        'id': id,
        'name': name,
        'host': host,
        'port': port,
        'enabled': enabled,
      };

  static ProbeTarget? fromJson(Object? raw) {
    if (raw is! Map) return null;
    final id = (raw['id'] as String? ?? '').trim();
    final host = (raw['host'] as String? ?? '').trim();
    final port = (raw['port'] as num?)?.toInt() ?? 0;
    if (id.isEmpty || host.isEmpty || port <= 0) return null;
    return ProbeTarget(
      id: id,
      name: (raw['name'] as String? ?? '').trim(),
      host: host,
      port: port,
      enabled: raw['enabled'] is bool ? raw['enabled'] as bool : true,
    );
  }
}

/// 探测结果（瞬时，不持久化）。
class ProbeResult {
  final bool ok;
  final int? rttMs;
  final int consecutiveFailures;
  final int consecutiveSuccesses;
  final int successCount;
  final int windowCount;

  /// 最近一次探测的时刻（毫秒）——页面用它显示「N 秒前检测」。
  final int? checkedAtMs;

  /// 本次连续失败的起点（毫秒）——从**连续失败的第一条**算，不是从健康度变红算，
  /// 否则 3 连败门槛会吞掉前两次的时间。非空表示当前处于超时中。
  final int? firstFailureAtMs;

  /// 上一次故障持续多久（毫秒，恢复时写入，只留最近一次）。
  /// 短于阈值的抖动会被忽略，不覆盖这个值。
  final int? lastOutageMs;

  const ProbeResult({
    required this.ok,
    this.rttMs,
    this.consecutiveFailures = 0,
    this.consecutiveSuccesses = 0,
    this.successCount = 0,
    this.windowCount = 20,
    this.checkedAtMs,
    this.firstFailureAtMs,
    this.lastOutageMs,
  });
}

/// 探测健康度的四态判据（评审定稿：连续次数与滑动窗口各管一类故障）。
///
/// 只用“连续失败”会漏掉间歇性故障：20 次里 10 成 10 败既不构成连续 3 败，
/// 也凑不出 90% 成功率，旧逻辑会让页面一直停在绿色。
///
/// - **down**（红）：连续失败 >= [probeDownAfter]，或 20 窗内失败 >= [probeWindowFailDown]；
/// - **ok**（绿）：连续成功 >= [probeOkAfter] 且窗口成功率 >= [probeOkRate]（首次进入）；
///   已经在 ok 时只需成功率不低于 [probeStayRate] 就能保持；
/// - **degraded**（黄）：窗口内失败 >= [probeWindowFailDegraded]，或从 ok 掉下 90%；
/// - **unknown**（灰）：样本不足 [probeMinSamples]、参数缺失、或观测中断。
///
/// 进入与退出用不同阈值（[probeRecoverRate] 严于 [probeStayRate]），避免临界限反复变色。
/// 页面文案必须写“TCP 探测成功率”——TCP 建连失败可能是目标没监听/被防火墙拒/
/// 连接数限制，不等于网络丢包。
const int probeDownAfter = 3;
const int probeWindowFailDown = 10;
const int probeWindowFailDegraded = 4;
const int probeOkAfter = 3;
const int probeMinSamples = 3;
const double probeOkRate = 0.9;
const double probeStayRate = 0.9;
const double probeRecoverRate = 0.95;

ProbeHealth deriveProbeHealth(
  ProbeHealth prev,
  ProbeResult? result, {
  bool observationGap = false,
}) {
  // 观测中断时不拿旧结果推断：中断期间发生了什么我们并不知道。
  if (observationGap || result == null) return ProbeHealth.unknown;
  if (result.windowCount < probeMinSamples) return ProbeHealth.unknown;

  final fails = result.windowCount - result.successCount;
  final rate = result.successCount / result.windowCount;
  final enoughSuccess = result.consecutiveSuccesses >= probeOkAfter;

  // 1) 明确故障：突发（连续失败）与长期劣化（窗口失败比例）各一条。
  if (result.consecutiveFailures >= probeDownAfter) return ProbeHealth.down;
  if (result.windowCount >= 10 && fails >= probeWindowFailDown) {
    return ProbeHealth.down;
  }

  // 2) 衰减/恢复：进档比保档严，防止临界抖动。
  switch (prev) {
    case ProbeHealth.down:
      if (enoughSuccess && rate >= probeRecoverRate) return ProbeHealth.ok;
      if (enoughSuccess && rate >= probeStayRate) return ProbeHealth.degraded;
      return ProbeHealth.down;
    case ProbeHealth.degraded:
      if (enoughSuccess && rate >= probeRecoverRate) return ProbeHealth.ok;
      return ProbeHealth.degraded;
    case ProbeHealth.ok:
      return rate >= probeStayRate ? ProbeHealth.ok : ProbeHealth.degraded;
    case ProbeHealth.unknown:
      if (enoughSuccess && rate >= probeOkRate) return ProbeHealth.ok;
      if (fails >= probeWindowFailDegraded) return ProbeHealth.degraded;
      return ProbeHealth.unknown;
  }
}

class ProbeEntry {
  final ProbeTarget target;

  /// null = 本次运行尚未检测（禁止用上次结果代当前）。
  final ProbeResult? result;

  /// 迟滞后的健康档位；null = 由 result 临时推导（演示数据用）。
  final ProbeHealth? health;

  const ProbeEntry({required this.target, this.result, this.health});
}

/// 打开 App 时是否应该自动开 VPN（纯函数，便于单测）。
///
/// 三条硬约束（缺一不可）：
///   1. 用户开了「启动时恢复上次状态」（默认关）；
///   2. 上次服务**确认**是运行状态（启动失败不会被记住）；
///   3. 至少有一个出口启用 —— 开了 VPN 却没有出口，等于给用户一个
///      “连着但什么都不通”的假象，不允许。
bool shouldAutoStartVpn({
  required bool restoreOnLaunch,
  required bool lastServiceOn,
  required bool hasAnyExit,
  required bool vpnRunning,
}) {
  if (vpnRunning) return false;
  if (!restoreOnLaunch || !lastServiceOn) return false;
  return hasAnyExit;
}

/// 两出口全关时必须恢复一个出口时选哪个：
/// 优先上次启用的那个；没有记录就回到公网代理（与上游 FlClash 默认一致）。
String resolveExitWhenAllOff(String? lastEnabledExit) {
  return lastEnabledExit == 'zerotier' ? 'zerotier' : 'mihomo';
}

/// 页面唯一数据源。
class NetworkState {
  final VpnStatus vpn;
  final ExitState mihomo;
  final ExitState zerotier;

  final String? ztIp;
  final int? ztLatencyMs;
  final LinkHealth linkHealth;

  /// null = 未知（未接线 / watchdog 未采样 / 数据已过期），UI 显示「—」，
  /// **不得**用 0 冒充真实值（设计文档 P3）。
  final int? silentForMs;
  final int? wireTxSinceRx;

  /// 自愈**尝试**次数：watchdog 发起重建的次数（不区分成败）。
  final int? selfHealCount;

  /// 自愈**成功**次数：RebuildSocket 返回 ok 的次数。
  /// ⚠️ 只代表 socket 换成了，**不代表链路恢复**（恢复看 [linkRecovery]）。
  final int? socketRebuildSuccesses;

  final int selfHealMax;

  /// 重建后的链路恢复确认：none / pending / confirmed / timeout。
  final String linkRecovery;

  /// 上一次检测到的“观测中断”时长（ms）：两次采样间隔 > 15s 说明页面/进程
  /// 期间没有在观测，不能拿这段时间的静默当故障。null = 没发生过。
  final int? observationGapMs;

  /// 对端列表被截断（core 报的数量超过包装层容量 64）。
  final bool peersTruncated;

  /// 截断时 core 报的真实对端数。
  final int? peersTotal;

  /// null = 拿不到（没有活节点 / 查询失败）；空列表 = 确实没有对端。
  /// ZT peer 是懒建立的，空列表不是故障。
  final List<PeerInfo>? peers;

  /// 本机最近真正打过交道的 ZT 地址（core 的 ARP 邻居表）。
  /// 只能作“添加探测目标”时的提示：客户端无法枚举网络成员 IP。
  final List<String> ztNeighborIps;

  /// 启动偏好：打开 App 时是否恢复上次 VPN 状态（设置页用）。
  final bool restoreOnLaunch;
  final List<ProbeEntry> probes;

  final String? lastError;
  final String nodeAddress;
  final String networkId;
  final int routesCount;
  final int? udpPort;
  final int? socketGen;

  /// 上次探测距今秒数；null = 未检测。
  final int? checkedAgoSec;

  const NetworkState({
    required this.vpn,
    required this.mihomo,
    required this.zerotier,
    this.ztIp,
    this.ztLatencyMs,
    this.linkHealth = LinkHealth.unknown,
    this.silentForMs,
    this.wireTxSinceRx,
    this.selfHealCount,
    this.socketRebuildSuccesses,
    this.selfHealMax = 10,
    this.linkRecovery = 'none',
    this.observationGapMs,
    this.peersTruncated = false,
    this.peersTotal,
    this.peers,
    this.ztNeighborIps = const [],
    this.restoreOnLaunch = false,
    this.probes = const [],
    this.lastError,
    this.nodeAddress = '',
    this.networkId = '',
    this.routesCount = 0,
    this.udpPort,
    this.socketGen,
    this.checkedAgoSec,
  });

  int get enabledExitCount =>
      (mihomo.enabled ? 1 : 0) + (zerotier.enabled ? 1 : 0);

  Iterable<PeerInfo> get _peerList => peers ?? const <PeerInfo>[];

  /// 基础设施对端（planet 根 / moon 根），不算“我的设备”。
  int get infraPeers => _peerList
      .where((p) => p.role == 'PLANET' || p.role == 'MOON')
      .length;

  /// 真实设备对端（摘要用这个数字）。
  int get devicePeers => _peerList.length - infraPeers;
}

/// 7 派生状态判定（严格按设计文档 §5 的优先级顺序）。
NetworkUiState deriveUiState(NetworkState state) {
  if (state.vpn != VpnStatus.on) {
    return NetworkUiState.s0VpnOff;
  }
  if (state.mihomo.status == ExitRunStatus.error ||
      state.zerotier.status == ExitRunStatus.error) {
    return NetworkUiState.s6StartFailed;
  }
  if (state.linkHealth == LinkHealth.stalled ||
      state.linkHealth == LinkHealth.rebuilding) {
    return NetworkUiState.s2LinkStalled;
  }
  if (state.zerotier.status == ExitRunStatus.running &&
      (state.ztIp == null || state.ztIp!.isEmpty)) {
    return NetworkUiState.s3WaitingConfig;
  }
  if (state.zerotier.status == ExitRunStatus.starting) {
    return NetworkUiState.s7Starting;
  }
  if (state.probes.isEmpty) {
    return NetworkUiState.s4NoProbe;
  }
  if (state.enabledExitCount <= 1) {
    return NetworkUiState.s5OnlyProxy;
  }
  return NetworkUiState.s1Normal;
}
