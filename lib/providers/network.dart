import 'dart:async';
import 'dart:convert';
import 'dart:io';

import 'package:fl_clash/common/common.dart';
import 'package:fl_clash/core/core.dart';
import 'package:fl_clash/enum/enum.dart';
import 'package:fl_clash/models/common.dart';
import 'package:fl_clash/models/network_probe.dart';
import 'package:fl_clash/models/network_state.dart';
import 'package:fl_clash/plugins/service.dart';
import 'package:fl_clash/providers/action.dart';
import 'package:fl_clash/providers/app.dart';
import 'package:fl_clash/providers/config.dart';
import 'package:fl_clash/providers/state.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:path/path.dart' as p;

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

  /// 启动偏好：打开 App 时是否恢复上次的 VPN 运行状态。
  ///
  /// 默认 **false**（安全默认）：新装用户不会莫名其妙地被连上 VPN。
  /// 开机自动开启不走这个开关，而是 Android 系统的「始终开启的 VPN」，
  /// 系统会在开机后直接发 VPN_START_REQUESTED（见 ServiceBroadcastReceiver）。
  final bool restoreOnLaunch;

  /// 上次「服务确认运行」的结果。只在服务真的起来之后写，点开关那一刻不写，
  /// 否则一次启动失败会被记成用户想开，之后就变成开机必开。
  final bool lastServiceOn;

  /// 最后一次处于启用状态的出口（`mihomo` / `zerotier` / null）。
  /// 「至少一个出口」硬约束靠它恢复，而不是替用户瞎猜。
  final String? lastEnabledExit;

  const NetworkIntent({
    required this.mihomoEnabled,
    required this.zerotierEnabled,
    this.probes = const [],
    this.restoreOnLaunch = false,
    this.lastServiceOn = false,
    this.lastEnabledExit,
  });

  /// 有没有任何出口被启用。两出口全关时**不得**开启 VPN（硬约束）。
  bool get hasAnyExit => mihomoEnabled || zerotierEnabled;

  NetworkIntent copyWith({
    bool? mihomoEnabled,
    bool? zerotierEnabled,
    List<ProbeTarget>? probes,
    bool? restoreOnLaunch,
    bool? lastServiceOn,
    String? lastEnabledExit,
  }) {
    return NetworkIntent(
      mihomoEnabled: mihomoEnabled ?? this.mihomoEnabled,
      zerotierEnabled: zerotierEnabled ?? this.zerotierEnabled,
      probes: probes ?? this.probes,
      restoreOnLaunch: restoreOnLaunch ?? this.restoreOnLaunch,
      lastServiceOn: lastServiceOn ?? this.lastServiceOn,
      lastEnabledExit: lastEnabledExit ?? this.lastEnabledExit,
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
      'startup': {'restoreOnLaunch': intent.restoreOnLaunch},
      'lastServiceOn': intent.lastServiceOn,
      if (intent.lastEnabledExit != null)
        'lastEnabledExit': intent.lastEnabledExit,
    });
    await file.writeAsString('$content\n', flush: true);
    await _mirrorIntentForNative(intent);
  } catch (_) {
    // 写失败不影响本次显示；下次刷新会再读一次。
  }
}

/// 把意图镜像到 SharedPreferences，供**不经过 Dart** 的启动路径使用
/// （开机时系统「始终开启的 VPN」会直接发 VPN_START_REQUESTED）。
/// 原生只关心“有没有出口”，用来执行「两出口全关就不开 VPN」的硬约束。
Future<void> _mirrorIntentForNative(NetworkIntent intent) async {
  try {
    await preferences.saveNetworkIntent(
      jsonEncode({
        'mihomoEnabled': intent.mihomoEnabled,
        'zerotierEnabled': intent.zerotierEnabled,
        'lastEnabledExit': intent.lastEnabledExit,
      }),
    );
  } catch (_) {
    // 镜像失败只影响开机守卫的保守程度（读不到就不拦），不影响 App 自身。
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
        final lastEnabledExit = _readString(json, 'lastEnabledExit');
        return NetworkIntent(
          mihomoEnabled: _readBool(json['mihomo'], 'enabled', true),
          zerotierEnabled: _readBool(json['zerotier'], 'enabled', true),
          probes: probes,
          restoreOnLaunch: _readBool(
            json['startup'],
            'restoreOnLaunch',
            false,
          ),
          lastServiceOn: _readBool(json, 'lastServiceOn', false),
          lastEnabledExit: lastEnabledExit.isEmpty ? null : lastEnabledExit,
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

  /// 观测连续性阈值（**不是**网络故障阈值）：两次采样间隔超过它，说明
  /// 页面在后台/进程被挂起，期间发生了什么我们不知道，一律复位基线。
  static const _observationGapAfter = Duration(seconds: 15);

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
  int? _lastProbePort;
  int? _lastSampleAtMs;
  int? _observationGapMs;

  // ZT wire 速率基线：两次成功快照的累计字节 + 时间。
  int? _lastRateAtMs;
  int? _lastRxBytesTotal;
  int? _lastTxBytesTotal;
  int? _ztRxBps;
  int? _ztTxBps;

  /// 上一次保存的探测端口，用于新增目标时预填。
  int? get lastProbePort => _lastProbePort;

  @override
  NetworkState build() {
    final page = ref.watch(currentPageLabelProvider);

    _timer?.cancel();
    _timer = null;
    _probeTimer?.cancel();
    _probeTimer = null;

    // 只在页面可见时轮询 / 探测（设计文档 §8：默认不做后台常驻查询）。
    //
    // 2026-11-26 修：仪表盘也可以放网络卡片（schema 合并布局），所以 dashboard
    // 同样算「可见」。之前只认 network → 在仪表盘上 peers / ZT 一直是空或旧数据，
    // 必须切到网络页才刷新。
    final watching =
        page == PageLabel.network || page == PageLabel.dashboard;
    if (watching) {
      _timer = Timer.periodic(_pollInterval, (_) => _refresh());
      _probeTimer = Timer.periodic(_probeInterval, (_) => _runProbes());
      // build() 内不能写 state，延到 microtask 再取一次数据。
      scheduleMicrotask(_refresh);
      scheduleMicrotask(_runProbes);
    }

    return _compose();
  }

  /// 观测连续性：间隔超过 [_observationGapAfter] 就视为“我们没在观测”。
  /// 此时复位所有探测基线（窗口/连续计数/健康档），而不是拿旧时间戳判故障。
  void _noteObservationGap() {
    final now = DateTime.now().millisecondsSinceEpoch;
    final last = _lastSampleAtMs;
    _lastSampleAtMs = now;
    if (last == null) return;
    final gapMs = now - last;
    if (gapMs <= _observationGapAfter.inMilliseconds) return;
    _observationGapMs = gapMs;
    for (final runtime in _probeRuntimes.values) {
      runtime.window.clear();
      runtime.consecutiveFailures = 0;
      runtime.consecutiveSuccesses = 0;
      runtime.health = ProbeHealth.unknown;
    }
  }

  /// 供卡片主动拉一次（目前没被调用，保留给以后的「展开即刷新」）。
  Future<void> refreshNow() => _refresh();

  Future<void> _refresh() async {
    _noteObservationGap();
    if (_refreshing) return;
    _refreshing = true;
    try {
      _intent ??= await _loadIntent();
      final vpn = _readVpnStatus();
      if (vpn == VpnStatus.on || vpn == VpnStatus.connecting) {
        try {
          _ztSnapshot = await coreController.getZeroTierStatus();
          _ztSnapshotAtMs = DateTime.now().millisecondsSinceEpoch;
          _ztKnown = true;
          _updateRates();
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

  /// 速率 = 两次成功快照的 wire 字节差 / 时间差（2s 轮询 → 约 2s 窗口）。
  ///
  /// 引擎/node 重建会让累计值归零，差值为负时一律当未知（显示「—」），
  /// 并把基线换成新读数——不然一个负数会污染后面每一次采样。
  void _updateRates() {
    final snapshot = _ztSnapshot;
    final at = _ztSnapshotAtMs;
    if (snapshot == null || at == null) return;
    final rx = _asNonNegativeInt(snapshot['wireRxBytes']);
    final tx = _asNonNegativeInt(snapshot['wireTxBytes']);
    final prevAt = _lastRateAtMs;
    final prevRx = _lastRxBytesTotal;
    final prevTx = _lastTxBytesTotal;
    _lastRateAtMs = at;
    _lastRxBytesTotal = rx;
    _lastTxBytesTotal = tx;
    if (prevAt == null || prevRx == null || prevTx == null) return;
    if (rx == null || tx == null) return;
    final dtSec = (at - prevAt) / 1000;
    if (dtSec < 0.2) return;
    final dRx = rx - prevRx;
    final dTx = tx - prevTx;
    _ztRxBps = dRx >= 0 ? (dRx / dtSec).round() : null;
    _ztTxBps = dTx >= 0 ? (dTx / dtSec).round() : null;
  }

  /// 速率文案：KB/s ↔ MB/s 自动切换（ZT 场景很少到 MB/s 级别）。
  String _formatRate(int? bps) {
    if (bps == null) return '—';
    if (bps >= 1024 * 1024) {
      final mb = bps / (1024 * 1024);
      return '${mb >= 10 ? mb.round() : mb.toStringAsFixed(1)} MB/s';
    }
    final kb = bps / 1024;
    return kb >= 100 ? '${kb.round()} KB/s' : '${kb.toStringAsFixed(1)} KB/s';
  }

  VpnStatus _readVpnStatus() {
    // App 尚未初始化完成时的读数没有意义。
    if (!ref.read(initProvider)) return VpnStatus.unknown;
    // ⚠️ 不能用 coreStatusProvider 当运行态：AppState.coreStatus 的默认值
    // 是 `connecting`，而 Android 的正常启停路径（SetupAction.setRunning）
    // 根本不写它 —— 只有仪表盘的「重启核心」会写。结果 coreStatus 永远
    // 停在 connecting，页面就表现为「一直在启动中、总开关点了不动」
    // （0.9.9 实机问题）。唯一可信的运行态是 SetupAction 的 isStartProvider。
    return ref.read(isStartProvider) ? VpnStatus.on : VpnStatus.off;
  }

  // ---- 业务探测 -----------------------------------------------------------

  /// 立即重测一次（页面右上角「重测」）。
  Future<void> retest() => _runProbes();

  // ---- 总开关 / 出口意图 -----------------------------------------------

  /// 总开关：唯一的 VPN 启停入口（服务级，不是出口级）。
  ///
  /// 硬约束：两个出口都关闭时**不**开 VPN —— 开了也没有任何出口，
  /// 只会给用户一个“连着但什么也不通”的假象。返回 false = 被规则拦下。
  Future<bool> setServiceEnabled(bool want) async {
    final intent = _intent ??= await _loadIntent();
    if (want && !intent.hasAnyExit) {
      return false;
    }
    // ⚠️ 必须走 SetupAction，不能直接调 Service().start()/stop()。
    // Service.start() 只叫 Android 服务起来，不启内核监听、也不改
    // isStartProvider/coreStatus —— 页面读到的运行态永远不变，
    // 表现为「总开关点了没反应、一直显示已开启」（0.9.9 实机问题）。
    await ref
        .read(setupActionProvider.notifier)
        .setRunning(want, initialize: want && !ref.read(initProvider));
    final running = ref.read(isStartProvider);
    if (running != want) {
      state = _compose();
      return false;
    }
    // 只有服务真的到达目标状态才落盘，避免「点了但没起来」被记成用户想开。
    _intent = intent.copyWith(lastServiceOn: running);
    final file = await _homeFile(_networkJsonName);
    if (file != null) {
      await _writeIntent(file, _intent!);
    }
    state = _compose();
    return true;
  }

  /// 出口意图：只写偏好，不直接启停内核。
  ///
  /// v1 的限制：mihomo/ZT 都随 VPN 服务一起起停，没有独立的运行开关，
  /// 所以改意图要**下次启动 VPN** 才生效（页面会提示）。
  Future<void> setExitEnabled({
    required bool zerotier,
    required bool enabled,
  }) async {
    final intent = _intent ??= await _loadIntent();
    _intent = intent.copyWith(
      mihomoEnabled: zerotier ? null : enabled,
      zerotierEnabled: zerotier ? enabled : null,
      lastEnabledExit: enabled ? (zerotier ? 'zerotier' : 'mihomo') : null,
    );
    final file = await _homeFile(_networkJsonName);
    if (file != null) {
      await _writeIntent(file, _intent!);
    }
    state = _compose();
  }

  /// 启动偏好：打开 App 时是否恢复上次状态。
  Future<void> setRestoreOnLaunch(bool value) async {
    final intent = _intent ??= await _loadIntent();
    _intent = intent.copyWith(restoreOnLaunch: value);
    final file = await _homeFile(_networkJsonName);
    if (file != null) {
      await _writeIntent(file, _intent!);
    }
    state = _compose();
  }

  /// 当前启用的出口数量（页面提示用）。
  int get enabledExitCount => _intent == null
      ? 0
      : (_intent!.mihomoEnabled ? 1 : 0) + (_intent!.zerotierEnabled ? 1 : 0);

  /// 新增或更新一个探测目标（持久化到 network.json）。
  Future<void> saveProbe(ProbeTarget target) async {
    _lastProbePort = target.port;
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
    String? mihomoTrafficLine;
    var mihomoStatus = ExitRunStatus.stopped;
    if (vpnActive) {
      // 与 _readVpnStatus 同一来源（isStartProvider），不再混用 coreStatus。
      mihomoStatus = vpnOn ? ExitRunStatus.running : ExitRunStatus.starting;
      if (vpnOn) {
        final mixedPort = ref.read(patchClashConfigProvider).mixedPort;
        // 2026-11-26 用户要求：去掉「mihomo ·」前缀；同一行后面补接收/发送，
        // 视觉上与 ZeroTier 那条实时速率对齐（复用同一个 l10n 串）。
        mihomoLines.add(l10n.networkMixedPort(mixedPort));
        final traffic = ref
            .read(trafficsProvider)
            .list
            .safeLast(const Traffic());
        mihomoTrafficLine = l10n.networkZtRates(
          _formatRate(traffic.down.round()),
          _formatRate(traffic.up.round()),
        );
      }
    }
    final mihomo = ExitState(
      name: 'mihomo',
      subtitle: 'mihomo',
      enabled: intent.mihomoEnabled,
      status: mihomoStatus,
      lines: mihomoLines,
      trafficLine: mihomoTrafficLine,
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
    // 实时速率单独一行（不占 ztLines 的 2 行额度，否则会被链路说明行挤掉）
    String? ztTrafficLine;
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
            ztTrafficLine = l10n.networkZtRates(
              _formatRate(_ztRxBps),
              _formatRate(_ztTxBps),
            );
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
        trafficLine: ztTrafficLine,
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
      observationGapMs: _observationGapMs,
      linkRecovery: (snapshot?['linkRecovery'] as String? ?? '').trim().isEmpty
          ? 'none'
          : (snapshot!['linkRecovery'] as String).trim(),
      selfHealMax: selfHealMax,
      peersTruncated: snapshot?['peersTruncated'] == true,
      peersTotal: _asNonNegativeInt(snapshot?['peersTotal']),
      peers: _mapPeers(snapshot),
      ztNeighborIps: _mapStringList(snapshot?['ztNeighborIps']),
      restoreOnLaunch: intent.restoreOnLaunch,
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
            preferredPaths: _asNonNegativeInt(item['preferredPaths']) ?? 0,
            version: (item['version'] as String? ?? '').trim(),
            role: (item['role'] as String? ?? '').trim().toUpperCase(),
            lastReceiveAtMs: _asNonNegativeInt(item['lastReceiveAtMs']),
          ),
    ];
  }

  List<String> _mapStringList(Object? raw) {
    if (raw is! List) return const [];
    return [
      for (final item in raw)
        if (item is String && item.trim().isNotEmpty) item.trim(),
    ];
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

// ---------------------------------------------------------------------------
// 启动偏好（App 启动时执行一次，见 main.dart）
// ---------------------------------------------------------------------------

/// 按启动偏好决定是否自动开 VPN。
///
/// 只在 App 启动时调一次；开机自启不走这里（由 Android 系统的「始终开启的
/// VPN」直接发 VPN_START_REQUESTED → ServiceBroadcastReceiver → 原生启动）。
/// 任何异常都不得阻塞 App 启动。
Future<void> maybeRestoreVpnOnLaunch(ProviderContainer container) async {
  try {
    final intent = await _loadIntent();
    await _mirrorIntentForNative(intent);
    final coreStatus = container.read(coreStatusProvider);
    final running =
        coreStatus == CoreStatus.connected || coreStatus == CoreStatus.connecting;
    if (!shouldAutoStartVpn(
      restoreOnLaunch: intent.restoreOnLaunch,
      lastServiceOn: intent.lastServiceOn,
      hasAnyExit: intent.hasAnyExit,
      vpnRunning: running,
    )) {
      return;
    }
    await Service().start();
  } catch (_) {
    // 启动偏好失败不影响进 App：状态由网络页/仪表盘如实展示。
  }
}
