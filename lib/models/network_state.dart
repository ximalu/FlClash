/// FlClashTier 网络状态模型（v1 原型）
///
/// 设计文档：kb/projects/flclashtier/network-state-model.md
///
/// 原则 P4：UI 只消费 NetworkState，不直接读 core。接真数据时只替换
/// providers/network.dart 里的构造逻辑，本文件与视图层不需要改。
library;

/// 服务层：系统 VPN 通道。
enum VpnStatus { off, connecting, on, unknown }

/// 运行层：单个出口的实际运行状态。
enum ExitRunStatus { notEnabled, stopped, starting, running, error }

/// 链路层：ZT 物理通道健康度（判据来自 engine.go 的 watchdog 常量）。
enum LinkHealth { ok, idle, stalled, rebuilding }

/// 对端路径类型。
enum PeerPath { direct, relay, unknown }

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

/// 原型阶段的状态场景（仅供调试切换；接真数据后删除）。
enum NetworkDemoScene { s0, s1, s2, s3, s4, s5, s6, s7 }

extension NetworkDemoSceneLabel on NetworkDemoScene {
  String get label {
    switch (this) {
      case NetworkDemoScene.s0:
        return 'S0 VPN 未运行';
      case NetworkDemoScene.s1:
        return 'S1 全部正常';
      case NetworkDemoScene.s2:
        return 'S2 链路断';
      case NetworkDemoScene.s3:
        return 'S3 等待网络配置';
      case NetworkDemoScene.s4:
        return 'S4 未配置探测';
      case NetworkDemoScene.s5:
        return 'S5 只开公网';
      case NetworkDemoScene.s6:
        return 'S6 启动失败';
      case NetworkDemoScene.s7:
        return 'S7 启动中';
    }
  }
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

  const ExitState({
    required this.name,
    required this.subtitle,
    required this.enabled,
    required this.status,
    this.lines = const [],
    this.switchDisabledReason,
  });
}

/// 对端（纯 core 事实：40bit address + 延迟 + 路径）。
class PeerInfo {
  final String address;
  final int? latencyMs;
  final PeerPath path;
  final String version;

  const PeerInfo({
    required this.address,
    this.latencyMs,
    this.path = PeerPath.unknown,
    this.version = '',
  });
}

/// 探测目标（持久化配置，name 由用户填写）。
class ProbeTarget {
  final String id;
  final String name;
  final String host;
  final int port;

  const ProbeTarget({
    required this.id,
    required this.name,
    required this.host,
    required this.port,
  });

  String get endpoint => '$host:$port';
}

/// 探测结果（瞬时，不持久化）。
class ProbeResult {
  final bool ok;
  final int? rttMs;
  final int consecutiveFailures;
  final int successCount;
  final int windowCount;

  const ProbeResult({
    required this.ok,
    this.rttMs,
    this.consecutiveFailures = 0,
    this.successCount = 0,
    this.windowCount = 20,
  });
}

class ProbeEntry {
  final ProbeTarget target;

  /// null = 本次运行尚未检测（禁止用上次结果代当前）。
  final ProbeResult? result;

  const ProbeEntry({required this.target, this.result});
}

/// 页面唯一数据源。
class NetworkState {
  final VpnStatus vpn;
  final ExitState mihomo;
  final ExitState zerotier;

  final String? ztIp;
  final int? ztLatencyMs;
  final LinkHealth linkHealth;
  final int silentForMs;
  final int wireTxSinceRx;
  final int selfHealCount;
  final int selfHealMax;

  final List<PeerInfo> peers;
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
    this.linkHealth = LinkHealth.ok,
    this.silentForMs = 0,
    this.wireTxSinceRx = 0,
    this.selfHealCount = 0,
    this.selfHealMax = 10,
    this.peers = const [],
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

  int get directPeers =>
      peers.where((p) => p.path == PeerPath.direct).length;

  int get relayPeers => peers.where((p) => p.path == PeerPath.relay).length;
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

// ---------------------------------------------------------------------------
// 原型假数据（接真数据后整段删除，连同 NetworkDemoScene）
// ---------------------------------------------------------------------------

const _mihomoRunning = ExitState(
  name: '公网代理',
  subtitle: 'mihomo',
  enabled: true,
  status: ExitRunStatus.running,
  lines: ['混合端口 7890 在监听'],
  switchDisabledReason: kMihomoLifecycleNote,
);

ExitState _zt({
  bool enabled = true,
  ExitRunStatus status = ExitRunStatus.running,
  List<String> lines = const [],
}) =>
    ExitState(
      name: '虚拟组网',
      subtitle: 'ZeroTier',
      enabled: enabled,
      status: status,
      lines: lines,
    );

const _probeI3 = ProbeTarget(
  id: 'p1',
  name: 'i3',
  host: '192.168.196.81',
  port: 4533,
);
const _probeNas = ProbeTarget(
  id: 'p2',
  name: 'NAS',
  host: '192.168.196.81',
  port: 2283,
);

const _peers3 = [
  PeerInfo(address: 'a5cd13296e', latencyMs: 12, path: PeerPath.direct),
  PeerInfo(address: '2444883c20', latencyMs: 41, path: PeerPath.direct),
  PeerInfo(address: '036b3f67f2', latencyMs: 128, path: PeerPath.relay),
];

NetworkState demoNetworkState(NetworkDemoScene scene) {
  const okProbes = [
    ProbeEntry(
      target: _probeI3,
      result: ProbeResult(
        ok: true,
        rttMs: 14,
        successCount: 20,
        windowCount: 20,
      ),
    ),
    ProbeEntry(
      target: _probeNas,
      result: ProbeResult(
        ok: true,
        rttMs: 9,
        successCount: 20,
        windowCount: 20,
      ),
    ),
  ];

  switch (scene) {
    case NetworkDemoScene.s1:
      return const NetworkState(
        vpn: VpnStatus.on,
        mihomo: _mihomoRunning,
        zerotier: ExitState(
          name: '虚拟组网',
          subtitle: 'ZeroTier',
          enabled: true,
          status: ExitRunStatus.running,
          lines: ['192.168.196.120 · 12 ms · 探测成功 20/20'],
        ),
        ztIp: '192.168.196.120',
        ztLatencyMs: 12,
        peers: _peers3,
        probes: okProbes,
        nodeAddress: '036b3f67f2',
        networkId: 'b6079f73c6c0eb31',
        routesCount: 1,
        udpPort: 9994,
        socketGen: 1,
        checkedAgoSec: 8,
      );

    case NetworkDemoScene.s0:
      return const NetworkState(
        vpn: VpnStatus.off,
        mihomo: ExitState(
          name: '公网代理',
          subtitle: 'mihomo',
          enabled: true,
          status: ExitRunStatus.stopped,
          switchDisabledReason: kMihomoLifecycleNote,
        ),
        zerotier: ExitState(
          name: '虚拟组网',
          subtitle: 'ZeroTier',
          enabled: true,
          status: ExitRunStatus.stopped,
        ),
        probes: [
          ProbeEntry(target: _probeI3),
          ProbeEntry(target: _probeNas),
        ],
        nodeAddress: '036b3f67f2',
        networkId: 'b6079f73c6c0eb31',
      );

    case NetworkDemoScene.s2:
      return NetworkState(
        vpn: VpnStatus.on,
        mihomo: _mihomoRunning,
        zerotier: _zt(
          status: ExitRunStatus.running,
          lines: ['无入站 68 秒（期间发了 41 个包）', '正在自动重建连接…（第 2/10 次）'],
        ),
        ztIp: '192.168.196.120',
        linkHealth: LinkHealth.rebuilding,
        silentForMs: 68000,
        wireTxSinceRx: 41,
        selfHealCount: 2,
        peers: _peers3,
        probes: okProbes,
        nodeAddress: '036b3f67f2',
        networkId: 'b6079f73c6c0eb31',
        routesCount: 1,
        udpPort: 9994,
        socketGen: 3,
        checkedAgoSec: 12,
      );

    case NetworkDemoScene.s3:
      return NetworkState(
        vpn: VpnStatus.on,
        mihomo: _mihomoRunning,
        zerotier: _zt(
          status: ExitRunStatus.running,
          lines: [
            '已加入网络 b6079f73c6…，尚未分配到 IP',
            '如果持续超过 1 分钟，请在 ZeroTier Central 确认该节点是否已授权',
          ],
        ),
        probes: [
          const ProbeEntry(
            target: _probeI3,
            result: ProbeResult(ok: false, consecutiveFailures: 6),
          ),
          const ProbeEntry(target: _probeNas),
        ],
        nodeAddress: '036b3f67f2',
        networkId: 'b6079f73c6c0eb31',
        udpPort: 9994,
        socketGen: 1,
        checkedAgoSec: 4,
      );

    case NetworkDemoScene.s4:
      return NetworkState(
        vpn: VpnStatus.on,
        mihomo: _mihomoRunning,
        zerotier: _zt(lines: const ['192.168.196.120 · 12 ms · 探测成功 20/20']),
        ztIp: '192.168.196.120',
        ztLatencyMs: 12,
        peers: _peers3,
        nodeAddress: '036b3f67f2',
        networkId: 'b6079f73c6c0eb31',
        routesCount: 1,
        udpPort: 9994,
        socketGen: 1,
      );

    case NetworkDemoScene.s5:
      return NetworkState(
        vpn: VpnStatus.on,
        mihomo: _mihomoRunning,
        zerotier: _zt(
          enabled: false,
          status: ExitRunStatus.notEnabled,
          lines: const ['已在设置中关闭 · 私网设备不可访问'],
        ),
        probes: okProbes,
        nodeAddress: '036b3f67f2',
        networkId: 'b6079f73c6c0eb31',
        checkedAgoSec: 8,
      );

    case NetworkDemoScene.s6:
      return NetworkState(
        vpn: VpnStatus.on,
        mihomo: _mihomoRunning,
        zerotier: _zt(
          status: ExitRunStatus.error,
          lines: const ['ZeroTier 端口 9994 已被占用'],
        ),
        lastError: 'ZeroTier 端口 9994 已被占用',
        probes: okProbes,
        nodeAddress: '036b3f67f2',
        networkId: 'b6079f73c6c0eb31',
        checkedAgoSec: 8,
      );

    case NetworkDemoScene.s7:
      return NetworkState(
        vpn: VpnStatus.on,
        mihomo: _mihomoRunning,
        zerotier: _zt(
          status: ExitRunStatus.starting,
          lines: const ['正在连接网络…'],
        ),
        probes: const [
          ProbeEntry(target: _probeI3),
          ProbeEntry(target: _probeNas),
        ],
        nodeAddress: '036b3f67f2',
        networkId: 'b6079f73c6c0eb31',
        udpPort: 9994,
      );
  }
}
