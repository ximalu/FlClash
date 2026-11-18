import 'dart:async';
import 'dart:convert';
import 'dart:io';

import 'package:fl_clash/common/common.dart';
import 'package:fl_clash/core/core.dart';
import 'package:fl_clash/enum/enum.dart';
import 'package:fl_clash/models/network_probe.dart';
import 'package:fl_clash/models/network_state.dart';
import 'package:fl_clash/providers/app.dart';
import 'package:fl_clash/providers/config.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:path/path.dart' as p;

/// 调试用的状态场景覆盖：`null` = 用真实数据，其余 = 强制显示该演示场景。
///
/// 保留它是为了真机排查（页面上没有别的办法稳定复现 S2/S6 这类罕见态）。
class NetworkDemoSceneController extends Notifier<NetworkDemoScene?> {
  @override
  NetworkDemoScene? build() => null;

  void select(NetworkDemoScene? scene) {
    state = scene;
  }
}

final networkDemoSceneProvider =
    NotifierProvider<NetworkDemoSceneController, NetworkDemoScene?>(
  NetworkDemoSceneController.new,
);

// ---------------------------------------------------------------------------
// 意图层：`<HomeDir>/network.json`（设计文档 §6.1，唯一意图源）
// ---------------------------------------------------------------------------

/// 用户希望哪个出口运行、想探测哪些目标。
///
/// **不得**用 mihomo 的 `Tun.enable` 推断意图（原则 P1）。
class NetworkIntent {
  final bool mihomoEnabled;
  final bool zerotierEnabled;
  final List<ProbeTarget> probes;

  const NetworkIntent({
    required this.mihomoEnabled,
    required this.zerotierEnabled,
    this.probes = const [],
  });

  NetworkIntent copyWith({
    bool? mihomoEnabled,
    bool? zerotierEnabled,
    List<ProbeTarget>? probes,
  }) {
    return NetworkIntent(
      mihomoEnabled: mihomoEnabled ?? this.mihomoEnabled,
      zerotierEnabled: zerotierEnabled ?? this.zerotierEnabled,
      probes: probes ?? this.probes,
    );
  }
}

const _networkJsonName = 'network.json';
const _zeroTierJsonName = 'zerotier.json';

/// ZeroTier Network ID 必须是 16 位十六进制（Go 侧 ParseNWID 同样要求）。
final _networkIdRegExp = RegExp(r'^[0-9a-fA-F]{16}$');

Future<File?> _homeFile(String name) async {
  try {
    final home = await appPath.homeDirPath;
    return File(p.join(home, name));
  } catch (_) {
    return null;
  }
}

String _readString(Map? json, String key) {
  final value = json?[key];
  return value is String ? value.trim() : '';
}

Map? _asMap(Object? value) => value is Map ? value : null;

bool _readBool(Object? json, String key, bool fallback) {
  final map = _asMap(json);
  final value = map?[key];
  return value is bool ? value : fallback;
}

Future<Map?> _readJson(File file) async {
  try {
    final raw = jsonDecode(await file.readAsString());
    return _asMap(raw);
  } catch (_) {
    return null;
  }
}

/// 旧语义（`Config.Enabled()` = network-id 非空）只用来做**一次性迁移**的默认值，
/// 迁移完成后意图只读 network.json——否则「启用但还没填 ID」无法表达。
Future<bool> _hasConfiguredNetworkId() async {
  final file = await _homeFile(_zeroTierJsonName);
  if (file == null || !await file.exists()) return false;
  final json = await _readJson(file);
  return _networkIdRegExp.hasMatch(_readString(json, 'network-id'));
}

Future<void> _writeIntent(File file, NetworkIntent intent) async {
  try {
    const encoder = JsonEncoder.withIndent('  ');
    final content = encoder.convert({
      'mihomo': {'enabled': intent.mihomoEnabled},
      'zerotier': {'enabled': intent.zerotierEnabled},
      'probes': [for (final probe in intent.probes) probe.toJson()],
    });
    await file.writeAsString('$content\n', flush: true);
  } catch (_) {
    // 写失败不影响本次显示；下次刷新会再读一次。
  }
}

/// 读取意图。文件缺失 → 按当前配置迁移出默认值并落盘；文件损坏 → 用默认值
/// 显示但**不覆盖**用户文件（宁可少显示，不静默改写配置）。
Future<NetworkIntent> _loadIntent() async {
  try {
    final file = await _homeFile(_networkJsonName);
    if (file != null && await file.exists()) {
      final json = await _readJson(file);
      if (json != null) {
        final probes = <ProbeTarget>[];
        final rawProbes = json['probes'];
        if (rawProbes is List) {
          for (final item in rawProbes) {
            final probe = ProbeTarget.fromJson(item);
            if (probe != null) probes.add(probe);
          }
        }
        return NetworkIntent(
          mihomoEnabled: _readBool(json['mihomo'], 'enabled', true),
          zerotierEnabled: _readBool(json['zerotier'], 'enabled', true),
          probes: probes,
        );
      }
      return const NetworkIntent(mihomoEnabled: true, zerotierEnabled: true);
    }
    final intent = NetworkIntent(
      mihomoEnabled: true,
      zerotierEnabled: await _hasConfiguredNetworkId(),
    );
    if (file != null) {
      await _writeIntent(file, intent);
    }
    return intent;
  } catch (_) {
    // 读失败也要能显示页面：用默认意图，不写盘、不覆盖用户配置。
    return const NetworkIntent(mihomoEnabled: true, zerotierEnabled: true);
  }
}

// ---------------------------------------------------------------------------
// 页面唯一数据源
// ---------------------------------------------------------------------------

/// 单个探测目标的运行时状态（瞬时，不持久化；重启清空）。
class _ProbeRuntime {
  final List<bool> window = <bool>[];
  int consecutiveFailures = 0;
  int consecutiveSuccesses = 0;
  int? rttMs;
  int? checkedAtMs;
  ProbeHealth health = ProbeHealth.unknown;

  ProbeResult result({required bool lastOk}) {
    return ProbeResult(
      ok: lastOk,
      rttMs: rttMs,
      consecutiveFailures: consecutiveFailures,
      consecutiveSuccesses: consecutiveSuccesses,
      successCount: window.where((ok) => ok).length,
      // 窗口里实际有多少次采样就报多少（刚开始显示 成功 3/3，满窗后 20/20），
      // 不用固定 20 冒充“已经有 20 次结果”。
      windowCount: window.length,
    );
  }
}

/// `NetworkState` 的唯一构造点（页面不直连 core，原则 P4）。
///
/// 数据来源：
///   - `vpn` ← `coreStatusProvider`（CoreStatus）
///   - `zerotier.*` ← `coreController.getZeroTierStatus()`（live Engine，2s）
///   - `mihomo.*` ← `coreStatusProvider` + `patchClashConfigProvider.mixedPort`
///   - `mihomo.enabled` / `zerotier.enabled` / `probes` ← `network.json`
///   - 业务探测 ← 本文件内部的 TCP connect 调度器（串行、单目标防重入）
class NetworkStateController extends Notifier<NetworkState> {
  /// 与仪表盘 `zerotier_status.dart` 一致的节奏。
  static const _pollInterval = Duration(seconds: 2);

  /// 探测周期：设计文档要求 10–30s，取 10s（只在页面可见 + VPN 运行时跑）。
  static const _probeInterval = Duration(seconds: 10);

  /// 单次 TCP connect 超时。
  static const _probeTimeout = Duration(seconds: 2);

  /// 每个目标的滑动窗口大小（成功率的 M）。
  static const _probeWindow = 20;

  /// 判据常量来自 `core/zerotier/engine.go`（UI 不得自创阈值）。
  static const _selfHealMax = 10;

  /// RPC 失败时会保留上一次成功读数，但超过这个时长就必须当未知。
  static const _staleAfter = Duration(seconds: 6);

  Timer? _timer;
  Timer? _probeTimer;
  bool _refreshing = false;
  bool _probing = false;
  final Set<String> _probingIds = <String>{};
  NetworkIntent? _intent;
  Map? _ztSnapshot;
  int? _ztSnapshotAtMs;
  bool _ztKnown = false;
  final Map<String, _ProbeRuntime> _probeRuntimes = <String, _ProbeRuntime>{};
  int? _lastProbeAtMs;

  @override
  NetworkState build() {
    final scene = ref.watch(networkDemoSceneProvider);
    final page = ref.watch(currentPageLabelProvider);

    _timer?.cancel();
    _timer = null;
    _probeTimer?.cancel();
    _probeTimer = null;

    if (scene != null) {
      return demoNetworkState(scene);
    }

    // 只在页面可见时轮询 / 探测（设计文档 §8：默认不做后台常驻查询）。
    if (page == PageLabel.network) {
      _timer = Timer.periodic(_pollInterval, (_) => _refresh());
      _probeTimer = Timer.periodic(_probeInterval, (_) => _runProbes());
      // build() 内不能写 state，延到 microtask 再取一次数据。
      scheduleMicrotask(_refresh);
      scheduleMicrotask(_runProbes);
    }

    return _compose();
  }

  Future<void> _refresh() async {
    if (_refreshing) return;
    _refreshing = true;
    try {
      if (ref.read(networkDemoSceneProvider) != null) return;
      _intent ??= await _loadIntent();
      final vpn = _readVpnStatus();
      if (vpn == VpnStatus.on || vpn == VpnStatus.connecting) {
        try {
          _ztSnapshot = await coreController.getZeroTierStatus();
          _ztSnapshotAtMs = DateTime.now().millisecondsSinceEpoch;
          _ztKnown = true;
        } catch (_) {
          // Core RPC 不可用：保留上一次成功读数，绝不编造状态（原则 P3）。
        }
      }
      if (vpn != VpnStatus.on && _probeRuntimes.isNotEmpty) {
        // 总开关断开 = 探测结果清空（§6.2），重新开启后必须重测。
        _probeRuntimes.clear();
        _lastProbeAtMs = null;
      }
      if (!ref.mounted) return;
      state = _compose(vpnStatus: vpn);
    } catch (_) {
      // 查询失败时保持上一次的 state（不写假值），轮询继续。
    } finally {
      _refreshing = false;
    }
  }

  VpnStatus _readVpnStatus() {
    // App 尚未初始化完成时，CoreStatus 的默认值（disconnected）没有意义。
    if (!ref.read(initProvider)) return VpnStatus.unknown;
    switch (ref.read(coreStatusProvider)) {
      case CoreStatus.disconnected:
        return VpnStatus.off;
      case CoreStatus.connecting:
        return VpnStatus.connecting;
      case CoreStatus.connected:
        return VpnStatus.on;
    }
  }

  // ---- 业务探测 -----------------------------------------------------------

  /// 立即重测一次（页面右上角「重测」）。
  Future<void> retest() => _runProbes();

  /// 新增或更新一个探测目标（持久化到 network.json）。
  Future<void> saveProbe(ProbeTarget target) async {
    final intent = _intent ??= await _loadIntent();
    final probes = [...intent.probes];
    final index = probes.indexWhere((probe) => probe.id == target.id);
    if (index >= 0) {
      probes[index] = target;
    } else {
      probes.add(target);
    }
    await _persistProbes(probes);
  }

  Future<void> removeProbe(String id) async {
    final intent = _intent ??= await _loadIntent();
    _probeRuntimes.remove(id);
    await _persistProbes(
      intent.probes.where((probe) => probe.id != id).toList(),
    );
  }

  Future<void> _persistProbes(List<ProbeTarget> probes) async {
    final current = _intent ??
        const NetworkIntent(mihomoEnabled: true, zerotierEnabled: true);
    _intent = current.copyWith(probes: probes);
    final file = await _homeFile(_networkJsonName);
    if (file != null) {
      await _writeIntent(file, _intent!);
    }
    if (!ref.mounted) return;
    state = _compose();
    scheduleMicrotask(_runProbes);
  }

  /// 一轮探测：**串行**执行（并发上限 1）+ 单目标防重入，只在页面可见且
  /// VPN 运行时跑。TCP connect 只证明端口可连，不代表业务正常。
  Future<void> _runProbes() async {
    if (_probing) return;
    if (ref.read(networkDemoSceneProvider) != null) return;
    _intent ??= await _loadIntent();
    if (ref.read(currentPageLabelProvider) != PageLabel.network) return;
    if (_readVpnStatus() != VpnStatus.on) return;
    final targets = (_intent?.probes ?? const <ProbeTarget>[])
        .where((target) => target.enabled && target.isValid)
        .toList();
    if (targets.isEmpty) return;

    _probing = true;
    try {
      for (final target in targets) {
        if (!ref.mounted) return;
        if (ref.read(currentPageLabelProvider) != PageLabel.network) return;
        await _probeOne(target);
      }
      _lastProbeAtMs = DateTime.now().millisecondsSinceEpoch;
      if (!ref.mounted) return;
      state = _compose();
    } finally {
      _probing = false;
    }
  }

  Future<void> _probeOne(ProbeTarget target) async {
    if (_probingIds.contains(target.id)) return; // 单目标防重入
    _probingIds.add(target.id);
    ProbeAttempt attempt;
    try {
      attempt = await tcpConnectProbe(
        target.host,
        target.port,
        timeout: _probeTimeout,
      );
    } finally {
      _probingIds.remove(target.id);
    }
    _recordProbe(target.id, ok: attempt.ok, rttMs: attempt.rttMs);
  }

  void _recordProbe(String id, {required bool ok, int? rttMs}) {
    final runtime = _probeRuntimes.putIfAbsent(id, () => _ProbeRuntime());
    runtime.window.add(ok);
    while (runtime.window.length > _probeWindow) {
      runtime.window.removeAt(0);
    }
    if (ok) {
      runtime.consecutiveSuccesses += 1;
      runtime.consecutiveFailures = 0;
      runtime.rttMs = rttMs;
    } else {
      runtime.consecutiveFailures += 1;
      runtime.consecutiveSuccesses = 0;
      runtime.rttMs = null;
    }
    runtime.checkedAtMs = DateTime.now().millisecondsSinceEpoch;
    runtime.health = deriveProbeHealth(
      runtime.health,
      runtime.result(lastOk: ok),
    );
  }

  List<ProbeEntry> _probeEntries(
    NetworkIntent intent, {
    required bool vpnOn,
  }) {
    return [
      for (final target in intent.probes)
        ProbeEntry(
          target: target,
          result: vpnOn && target.enabled ? _probeResult(target.id) : null,
          health: vpnOn && target.enabled
              ? _probeRuntimes[target.id]?.health
              : null,
        ),
    ];
  }

  ProbeResult? _probeResult(String id) {
    final runtime = _probeRuntimes[id];
    if (runtime == null || runtime.checkedAtMs == null) return null;
    return runtime.result(
      lastOk: (runtime.window.isNotEmpty && runtime.window.last),
    );
  }

  // ---- 组装 ---------------------------------------------------------------

  NetworkState _compose({VpnStatus? vpnStatus}) {
    final l10n = currentAppLocalizations;
    final vpn = vpnStatus ?? _readVpnStatus();
    final intent = _intent ??
        const NetworkIntent(mihomoEnabled: true, zerotierEnabled: true);
    final vpnActive = vpn == VpnStatus.on || vpn == VpnStatus.connecting;
    final vpnOn = vpn == VpnStatus.on;

    // ---- 公网代理（mihomo）：开关永远置灰（本期不支持独立启停，§7） ----
    final mihomoLines = <String>[];
    var mihomoStatus = ExitRunStatus.stopped;
    if (vpnActive) {
      switch (ref.read(coreStatusProvider)) {
        case CoreStatus.connected:
          mihomoStatus = ExitRunStatus.running;
          final mixedPort = ref.read(patchClashConfigProvider).mixedPort;
          mihomoLines.add('mihomo · ${l10n.networkMixedPort(mixedPort)}');
        case CoreStatus.connecting:
          mihomoStatus = ExitRunStatus.starting;
        case CoreStatus.disconnected:
          mihomoStatus = ExitRunStatus.stopped;
      }
    }
    final mihomo = ExitState(
      name: 'mihomo',
      subtitle: 'mihomo',
      enabled: intent.mihomoEnabled,
      status: mihomoStatus,
      lines: mihomoLines,
      switchDisabledReason: kMihomoLifecycleNote,
    );

    // ---- 虚拟组网（ZeroTier） ----
    final snapshot = _liveSnapshot();
    final rawState = (snapshot?['state'] as String? ?? '').toUpperCase();
    final ip = (snapshot?['ipv4'] as String? ?? '').trim();
    final networkId = (snapshot?['networkId'] as String? ?? '').trim();
    final nodeAddress = (snapshot?['nodeAddress'] as String? ?? '').trim();
    final routes = _asNonNegativeInt(snapshot?['routes']) ?? 0;
    final linkHealth = _mapLinkHealth(snapshot);
    final silentForMs = _asNonNegativeInt(snapshot?['silentForMs']);
    final wireTxSinceRx = _asNonNegativeInt(snapshot?['wireTxSinceRx']);
    final selfHealAttempts = _asNonNegativeInt(snapshot?['selfHealAttempts']);
    final selfHealMax =
        _asNonNegativeInt(snapshot?['selfHealMax']) ?? _selfHealMax;

    // 链路层的说明行：只用 Go 算好的判据 + 真实计数。
    final linkLines = <String>[];
    if (linkHealth == LinkHealth.stalled) {
      if (silentForMs != null && wireTxSinceRx != null) {
        linkLines.add(
          l10n.networkZtLinkStalled((silentForMs / 1000).round(), wireTxSinceRx),
        );
      }
      if (selfHealAttempts != null && selfHealAttempts >= selfHealMax) {
        linkLines.add(l10n.networkZtHealExhausted);
      }
    } else if (linkHealth == LinkHealth.rebuilding &&
        selfHealAttempts != null) {
      linkLines.add(l10n.networkZtRebuilding(selfHealAttempts, selfHealMax));
    }

    final ztLines = <String>[...linkLines];
    late ExitState zerotier;
    if (!intent.zerotierEnabled) {
      zerotier = ExitState(
        name: 'ZeroTier',
        subtitle: 'ZeroTier',
        enabled: false,
        status: ExitRunStatus.notEnabled,
        lines: [l10n.networkZtDisabledLine],
      );
    } else if (!vpnActive) {
      zerotier = const ExitState(
        name: 'ZeroTier',
        subtitle: 'ZeroTier',
        enabled: true,
        status: ExitRunStatus.stopped,
      );
    } else {
      var status = ExitRunStatus.stopped;
      switch (rawState) {
        case 'RUNNING':
          status = ExitRunStatus.running;
          if (ip.isNotEmpty) {
            ztLines.add(ip);
          } else {
            // S3：不得写成「未授权」（§9.1）。
            ztLines.add(l10n.networkZtWaitingIp(_shortHex(networkId)));
            if (linkLines.isEmpty) {
              ztLines.add(l10n.networkZtWaitingHint);
            }
          }
        case 'STARTING':
          status = ExitRunStatus.starting;
          ztLines.add(l10n.networkZtStarting);
        case 'STOPPING':
        case 'STOPPED':
          status = ExitRunStatus.stopped;
        default:
          status = ExitRunStatus.stopped;
      }
      zerotier = ExitState(
        name: 'ZeroTier',
        subtitle: 'ZeroTier',
        enabled: true,
        status: status,
        lines: ztLines,
      );
    }

    final lastError = (snapshot?['lastError'] as String? ?? '').trim();

    return NetworkState(
      vpn: vpn,
      mihomo: mihomo,
      zerotier: zerotier,
      ztIp: ip.isEmpty ? null : ip,
      ztLatencyMs: null,
      linkHealth: linkHealth,
      silentForMs: silentForMs,
      wireTxSinceRx: wireTxSinceRx,
      selfHealCount: selfHealAttempts,
      socketRebuildSuccesses: _asNonNegativeInt(
        snapshot?['socketRebuildSuccesses'],
      ),
      linkRecovery: (snapshot?['linkRecovery'] as String? ?? '').trim().isEmpty
          ? 'none'
          : (snapshot!['linkRecovery'] as String).trim(),
      selfHealMax: selfHealMax,
      peersTruncated: snapshot?['peersTruncated'] == true,
      peersTotal: _asNonNegativeInt(snapshot?['peersTotal']),
      peers: _mapPeers(snapshot),
      probes: _probeEntries(intent, vpnOn: vpnOn),
      lastError: lastError.isEmpty ? null : lastError,
      nodeAddress: nodeAddress,
      networkId: networkId,
      routesCount: routes,
      udpPort: _asNonNegativeInt(snapshot?['udpPort']),
      socketGen: _asNonNegativeInt(snapshot?['socketGen']),
      checkedAgoSec: _checkedAgoSec(),
    );
  }

  int? _checkedAgoSec() {
    final at = _lastProbeAtMs;
    if (at == null) return null;
    final age = DateTime.now().millisecondsSinceEpoch - at;
    return age < 0 ? 0 : (age / 1000).round();
  }

  /// 上一次成功的读数；超过 [_staleAfter] 没拿到新快照就当作未知。
  Map? _liveSnapshot() {
    final at = _ztSnapshotAtMs;
    final raw = _ztSnapshot;
    if (!_ztKnown || at == null || raw == null) return null;
    final age = DateTime.now().millisecondsSinceEpoch - at;
    if (age > _staleAfter.inMilliseconds) return null;
    return raw;
  }

  LinkHealth _mapLinkHealth(Map? snapshot) {
    switch ((snapshot?['linkHealth'] as String? ?? '').toLowerCase()) {
      case 'ok':
        return LinkHealth.ok;
      case 'idle':
        return LinkHealth.idle;
      case 'stalled':
        return LinkHealth.stalled;
      case 'rebuilding':
        return LinkHealth.rebuilding;
      default:
        return LinkHealth.unknown;
    }
  }

  /// null = 未知（无快照 / 字段缺失）；空列表 = 确实没有对端。
  List<PeerInfo>? _mapPeers(Map? snapshot) {
    if (snapshot == null) return null;
    final raw = snapshot['peers'];
    if (raw is! List) return null;
    return [
      for (final item in raw)
        if (item is Map)
          PeerInfo(
            address: (item['address'] as String? ?? '').trim(),
            latencyMs: _asNonNegativeInt(item['latencyMs']),
            path: _mapPeerPath(item['path'] as String?),
            version: (item['version'] as String? ?? '').trim(),
          ),
    ];
  }

  PeerPath _mapPeerPath(String? path) {
    switch ((path ?? '').toLowerCase()) {
      case 'direct':
        return PeerPath.direct;
      case 'relay':
        return PeerPath.relay;
      default:
        return PeerPath.unknown;
    }
  }

  int? _asNonNegativeInt(Object? value) {
    if (value is! num) return null;
    final int v = value.toInt();
    return v < 0 ? null : v;
  }

  /// 省略号截断（如 `b6079f73c6c0…`）；空值原样返回，UI 会显示 `—`。
  String _shortHex(String value) {
    if (value.length <= 12) return value;
    return '${value.substring(0, 12)}…';
  }
}

final networkStateProvider =
    NotifierProvider<NetworkStateController, NetworkState>(
  NetworkStateController.new,
);
