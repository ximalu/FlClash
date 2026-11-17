import 'dart:async';
import 'dart:convert';
import 'dart:io';

import 'package:fl_clash/common/common.dart';
import 'package:fl_clash/core/core.dart';
import 'package:fl_clash/enum/enum.dart';
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

/// 用户希望哪个出口运行。**不得**用 mihomo 的 `Tun.enable` 推断（原则 P1）。
class NetworkIntent {
  final bool mihomoEnabled;
  final bool zerotierEnabled;

  const NetworkIntent({
    required this.mihomoEnabled,
    required this.zerotierEnabled,
  });
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

bool _readBool(Map? json, String key, bool fallback) {
  final value = json?[key];
  return value is bool ? value : fallback;
}

Future<Map?> _readJson(File file) async {
  try {
    final raw = jsonDecode(await file.readAsString());
    return raw is Map ? raw : null;
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
      'probes': const <Object>[],
    });
    await file.writeAsString('$content\n', flush: true);
  } catch (_) {
    // 写失败不影响本次显示；下次刷新会再读一次。
  }
}

/// 读取意图。文件缺失 → 按当前配置迁移出默认值并落盘；文件损坏 → 用默认值
/// 显示但**不覆盖**用户文件（宁可少显示，不静默改写配置）。
Future<NetworkIntent> _loadIntent() async {
  final file = await _homeFile(_networkJsonName);
  if (file != null && await file.exists()) {
    final json = await _readJson(file);
    if (json != null) {
      return NetworkIntent(
        mihomoEnabled: _readBool(json['mihomo'] as Map?, 'enabled', true),
        zerotierEnabled: _readBool(json['zerotier'] as Map?, 'enabled', true),
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
}

// ---------------------------------------------------------------------------
// 页面唯一数据源
// ---------------------------------------------------------------------------

/// `NetworkState` 的唯一构造点（页面不直连 core，原则 P4）。
///
/// 数据来源（本步能拿到的部分）：
///   - `vpn` ← `coreStatusProvider`（CoreStatus）
///   - `zerotier.*` ← `coreController.getZeroTierStatus()`（live Engine）
///   - `mihomo.*` ← `coreStatusProvider` + `patchClashConfigProvider.mixedPort`
///   - `mihomo.enabled` / `zerotier.enabled` ← `network.json`（唯一意图源）
/// 链路层（health/silent/gen/selfHeal）与 peers 仍未导出（第 2 步），
/// 业务探测仍是空列表（第 3 步）——缺的字段一律留空，UI 显示 `—`，不回落成「正常」。
class NetworkStateController extends Notifier<NetworkState> {
  /// 与 `lib/views/dashboard/widgets/zerotier_status.dart` 的节奏一致（2s）。
  static const _pollInterval = Duration(seconds: 2);

  /// 判据常量来自 `core/zerotier/engine.go`（UI 不得自创阈值）。
  static const _selfHealMax = 10;

  Timer? _timer;
  bool _refreshing = false;
  NetworkIntent? _intent;
  Map? _ztSnapshot;
  bool _ztKnown = false;

  @override
  NetworkState build() {
    final scene = ref.watch(networkDemoSceneProvider);
    final page = ref.watch(currentPageLabelProvider);

    _timer?.cancel();
    _timer = null;

    if (scene != null) {
      return demoNetworkState(scene);
    }

    // 只在页面可见时轮询（设计文档 §8：默认不做后台常驻查询）。
    if (page == PageLabel.network) {
      _timer = Timer.periodic(_pollInterval, (_) => _refresh());
    }
    // build() 内不能写 state，延到 microtask 再取一次数据。
    scheduleMicrotask(_refresh);

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
          _ztKnown = true;
        } catch (_) {
          // Core RPC 不可用：保留上一次成功读数，绝不编造状态（原则 P3）。
        }
      }
      if (!ref.mounted) return;
      state = _compose(vpnStatus: vpn);
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

  NetworkState _compose({VpnStatus? vpnStatus}) {
    final l10n = currentAppLocalizations;
    final vpn = vpnStatus ?? _readVpnStatus();
    final intent = _intent ??
        const NetworkIntent(mihomoEnabled: true, zerotierEnabled: true);
    final vpnActive = vpn == VpnStatus.on || vpn == VpnStatus.connecting;

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
    final snapshot = _ztKnown ? _ztSnapshot : null;
    final rawState = (snapshot?['state'] as String? ?? '').toUpperCase();
    final ip = (snapshot?['ipv4'] as String? ?? '').trim();
    final networkId = (snapshot?['networkId'] as String? ?? '').trim();
    final nodeAddress = (snapshot?['nodeAddress'] as String? ?? '').trim();
    final routes = ((snapshot?['routes'] as num?) ?? 0).toInt();

    final ztLines = <String>[];
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
      // 偏好保留、运行态清空（§6.2.1）：开关显示 intent，正文由 UI 补「VPN 未运行」。
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
            // S3：已入网但还没拿到 IP。**不得**写成「未授权」——客户端判不出来（§9.1）。
            ztLines.add(l10n.networkZtWaitingIp(_shortHex(networkId)));
            ztLines.add(l10n.networkZtWaitingHint);
          }
        case 'STARTING':
          status = ExitRunStatus.starting;
          ztLines.add(l10n.networkZtStarting);
        case 'STOPPING':
        case 'STOPPED':
          status = ExitRunStatus.stopped;
        default:
          // 读取不到引擎状态（RPC 未就绪/查询失败）时不猜，按「已停止」中性展示。
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

    return NetworkState(
      vpn: vpn,
      mihomo: mihomo,
      zerotier: zerotier,
      ztIp: ip.isEmpty ? null : ip,
      ztLatencyMs: null,
      // 链路层还没接线（第 2 步）：不能声称 ok。idle = 无判据，UI 也不会由此进 S2。
      linkHealth: LinkHealth.idle,
      silentForMs: 0,
      wireTxSinceRx: 0,
      selfHealCount: 0,
      selfHealMax: _selfHealMax,
      peers: const [],
      probes: const [],
      lastError: null,
      nodeAddress: nodeAddress,
      networkId: networkId,
      routesCount: routes,
      udpPort: null,
      socketGen: null,
      checkedAgoSec: null,
    );
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
