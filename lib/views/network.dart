import 'package:fl_clash/common/common.dart';
import 'package:fl_clash/models/network_state.dart';
import 'package:fl_clash/providers/network.dart';
import 'package:fl_clash/state.dart';
import 'package:fl_clash/widgets/widgets.dart';
import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

/// 静态原型：数据全部来自 providers/network.dart 的演示实现。
///
/// 界面规范：kb/projects/flclashtier/network-ui-states.md
/// 状态模型：kb/projects/flclashtier/network-state-model.md
///
/// 本页只消费 NetworkState，不读 core（原则 P4）。
/// 文案已 l10n 化；以下两处仍是原型专用、接真数据时会一起移除：
///   - NetworkDemoScene.label（调试菜单里的 S0–S7 名称）
///   - model 里的假数据行（如「无入站 68 秒…」）——那是示例载荷，不是界面文案
class NetworkView extends ConsumerWidget {
  const NetworkView({super.key});

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final l10n = context.appLocalizations;
    final state = ref.watch(networkStateProvider);
    final uiState = deriveUiState(state);
    return CommonScaffold(
      title: l10n.network,
      actions: [
        IconButton(
          tooltip: l10n.networkRetest,
          icon: const Icon(Icons.refresh),
          onPressed: uiState == NetworkUiState.s0VpnOff
              ? null
              : () => ref.read(networkStateProvider.notifier).retest(),
        ),
        const _DemoSceneButton(),
      ],
      body: ListView(
        padding: const EdgeInsets.fromLTRB(12, 8, 12, 24),
        children: [
          if (ref.watch(networkDemoSceneProvider) != null)
            Padding(
              padding: const EdgeInsets.only(bottom: 12, left: 4),
              child: Text(
                l10n.networkPrototypeBanner,
                style: context.textTheme.labelSmall?.copyWith(
                  color: context.colorScheme.outline,
                ),
              ),
            ),
          _SectionTitle(l10n.networkVpnService),
          _ServiceCard(state: state),
          const SizedBox(height: 24),
          _SectionTitle(l10n.networkExits),
          _ExitCard(
            state: state,
            exit: state.mihomo,
            uiState: uiState,
            isZeroTier: false,
          ),
          const SizedBox(height: 8),
          _ExitCard(
            state: state,
            exit: state.zerotier,
            uiState: uiState,
            isZeroTier: true,
          ),
          const SizedBox(height: 24),
          _SectionTitle(l10n.networkProbes),
          _ProbeCard(state: state),
          const SizedBox(height: 24),
          _PeersCard(state: state),
          const SizedBox(height: 8),
          _DiagnosticsCard(state: state),
        ],
      ),
    );
  }
}

void _notWired(BuildContext context) {
  ScaffoldMessenger.of(context).showSnackBar(
    SnackBar(
      content: Text(context.appLocalizations.networkPrototypeAction),
      duration: const Duration(seconds: 2),
    ),
  );
}

enum _Tone { pass, warn, error, neutral }

Color _toneColor(BuildContext context, _Tone tone) {
  final scheme = context.colorScheme;
  switch (tone) {
    case _Tone.pass:
      return scheme.primary;
    case _Tone.warn:
      return scheme.tertiary;
    case _Tone.error:
      return scheme.error;
    case _Tone.neutral:
      return scheme.outline;
  }
}

class _SectionTitle extends StatelessWidget {
  final String text;

  const _SectionTitle(this.text);

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.only(left: 4, bottom: 8),
      child: Text(
        text,
        style: context.textTheme.titleSmall?.copyWith(
          color: context.colorScheme.onSurfaceVariant,
        ),
      ),
    );
  }
}

class _Badge extends StatelessWidget {
  final String text;
  final _Tone tone;

  const _Badge({required this.text, required this.tone});

  @override
  Widget build(BuildContext context) {
    final color = _toneColor(context, tone);
    return Container(
      padding: const EdgeInsets.symmetric(horizontal: 6, vertical: 2),
      decoration: BoxDecoration(
        border: Border.all(color: color.withAlpha(120)),
        borderRadius: BorderRadius.circular(6),
      ),
      child: Text(
        text,
        maxLines: 1,
        overflow: TextOverflow.ellipsis,
        style: context.textTheme.labelSmall?.copyWith(color: color),
      ),
    );
  }
}

class _ServiceCard extends StatelessWidget {
  final NetworkState state;

  const _ServiceCard({required this.state});

  @override
  Widget build(BuildContext context) {
    final l10n = context.appLocalizations;
    final scheme = context.colorScheme;
    final on = state.vpn == VpnStatus.on;
    final subtitle = on
        ? l10n.networkServiceConnected(state.enabledExitCount)
        : l10n.networkServiceOff;
    return Card(
      margin: EdgeInsets.zero,
      child: ListTile(
        title: Text(l10n.networkVpnService),
        subtitle: Text(
          subtitle,
          style: context.textTheme.bodySmall?.copyWith(color: scheme.outline),
        ),
        trailing: Switch(
          value: on,
          onChanged: (_) => _notWired(context),
        ),
      ),
    );
  }
}

class _ExitBadgeData {
  final String text;
  final _Tone tone;

  const _ExitBadgeData(this.text, this.tone);
}

_ExitBadgeData _badgeFor({
  required BuildContext context,
  required ExitState exit,
  required NetworkUiState uiState,
  required bool isZeroTier,
}) {
  final l10n = context.appLocalizations;
  if (uiState == NetworkUiState.s0VpnOff) {
    return _ExitBadgeData(l10n.networkStopped, _Tone.neutral);
  }
  switch (exit.status) {
    case ExitRunStatus.error:
      return _ExitBadgeData(l10n.networkStartFailed, _Tone.error);
    case ExitRunStatus.notEnabled:
      return _ExitBadgeData(l10n.networkNotEnabled, _Tone.warn);
    case ExitRunStatus.starting:
      return _ExitBadgeData(l10n.networkStarting, _Tone.warn);
    case ExitRunStatus.stopped:
      return _ExitBadgeData(l10n.networkStopped, _Tone.neutral);
    case ExitRunStatus.running:
      if (!isZeroTier) {
        return _ExitBadgeData(l10n.networkRunning, _Tone.pass);
      }
      if (uiState == NetworkUiState.s2LinkStalled) {
        return _ExitBadgeData(l10n.networkLinkDown, _Tone.error);
      }
      if (uiState == NetworkUiState.s3WaitingConfig) {
        return _ExitBadgeData(l10n.networkWaitingConfig, _Tone.warn);
      }
      return _ExitBadgeData(l10n.networkDirect, _Tone.pass);
  }
}

String _peerSummary(BuildContext context, NetworkState state) {
  final l10n = context.appLocalizations;
  final peers = state.peers;
  // null = 拿不到（未知）；空列表 = 确实没有对端（ZT peer 懒建立，不是故障）。
  if (peers == null) {
    return '—';
  }
  if (peers.isEmpty) {
    return l10n.networkNoPeers;
  }
  // core 的 C API 给不出可靠的 direct/relay 证据（见 models 里 PeerStatus 的
  // Path 注释）。摘要只数真实设备（LEAF）：planet 根 / moon 根是基础设施，
  // 混进“对端”里会让 8 个对端看起来像 8 台设备。
  if (state.peersTruncated) {
    return l10n.networkPeersTruncated(state.peersTotal ?? peers.length);
  }
  if (state.unknownPeers > 0) {
    return l10n.networkPeersSummaryUnknown(state.devicePeers);
  }
  return l10n.networkPeersSummary(
    state.devicePeers,
    state.directPeers,
    state.relayPeers,
  );
}

/// 对端副行：版本 · 角色（· 最后收包在 N 分钟前，只在超过 1 分钟时显示）。
String _peerDetail(BuildContext context, PeerInfo peer) {
  final parts = <String>[];
  if (peer.version.isNotEmpty) {
    parts.add('v${peer.version}');
  }
  if (peer.role.isNotEmpty) {
    parts.add(peer.role);
  }
  final lastRx = peer.lastReceiveAtMs;
  if (lastRx != null && lastRx > 0) {
    final ageMs = DateTime.now().millisecondsSinceEpoch - lastRx;
    if (ageMs >= 60000) {
      parts.add(
        context.appLocalizations.networkPeerStale((ageMs / 60000).round()),
      );
    }
  }
  return parts.join(' · ');
}

String? _disabledReason({
  required BuildContext context,
  required ExitState exit,
  required NetworkUiState uiState,
  required bool isZeroTier,
}) {
  final l10n = context.appLocalizations;
  final vpnOff = uiState == NetworkUiState.s0VpnOff;
  if (exit.switchDisabledReason != null) {
    return vpnOff && !isZeroTier
        ? l10n.networkReasonVpnOff(l10n.networkProxyLifecycleNote)
        : l10n.networkProxyLifecycleNote;
  }
  return vpnOff ? l10n.networkVpnNotRunning : null;
}

class _ExitCard extends StatelessWidget {
  final NetworkState state;
  final ExitState exit;
  final NetworkUiState uiState;
  final bool isZeroTier;

  const _ExitCard({
    required this.state,
    required this.exit,
    required this.uiState,
    required this.isZeroTier,
  });

  @override
  Widget build(BuildContext context) {
    final l10n = context.appLocalizations;
    final scheme = context.colorScheme;
    final textTheme = context.textTheme;
    final badge = _badgeFor(
      context: context,
      exit: exit,
      uiState: uiState,
      isZeroTier: isZeroTier,
    );
    final disabledReason = _disabledReason(
      context: context,
      exit: exit,
      uiState: uiState,
      isZeroTier: isZeroTier,
    );
    final vpnOff = uiState == NetworkUiState.s0VpnOff;
    final showPeerSummary =
        isZeroTier && exit.status == ExitRunStatus.running && !vpnOff;
    final title =
        isZeroTier ? l10n.networkVirtualNetwork : l10n.networkProxy;

    return Card(
      margin: EdgeInsets.zero,
      child: Padding(
        padding: const EdgeInsets.fromLTRB(16, 12, 8, 12),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Row(
              children: [
                Expanded(
                  child: Column(
                    crossAxisAlignment: CrossAxisAlignment.start,
                    children: [
                      Row(
                        children: [
                          Flexible(
                            child: Text(
                              title,
                              maxLines: 1,
                              overflow: TextOverflow.ellipsis,
                              style: textTheme.titleMedium,
                            ),
                          ),
                          const SizedBox(width: 8),
                          _Badge(text: badge.text, tone: badge.tone),
                        ],
                      ),
                      const SizedBox(height: 2),
                      Text(
                        exit.subtitle,
                        style: textTheme.bodySmall?.copyWith(
                          color: scheme.outline,
                        ),
                      ),
                    ],
                  ),
                ),
                Switch(
                  value: exit.enabled,
                  onChanged: disabledReason == null
                      ? (_) => _notWired(context)
                      : null,
                ),
              ],
            ),
            for (final line in exit.lines.take(2))
              Padding(
                padding: const EdgeInsets.only(top: 6, right: 8),
                child: Text(line, style: textTheme.bodySmall),
              ),
            if (vpnOff && isZeroTier)
              Padding(
                padding: const EdgeInsets.only(top: 6, right: 8),
                child: Text(
                  l10n.networkIntentKept,
                  style: textTheme.bodySmall?.copyWith(color: scheme.outline),
                ),
              ),
            if (showPeerSummary)
              Padding(
                padding: const EdgeInsets.only(top: 6, right: 8),
                child: Text(
                  _peerSummary(context, state),
                  style: textTheme.bodySmall?.copyWith(color: scheme.outline),
                ),
              ),
            if (disabledReason != null)
              Padding(
                padding: const EdgeInsets.only(top: 8, right: 8),
                child: Row(
                  children: [
                    Icon(
                      Icons.lock_outline,
                      size: 13,
                      color: scheme.outline,
                    ),
                    const SizedBox(width: 4),
                    Expanded(
                      child: Text(
                        disabledReason,
                        style: textTheme.labelSmall?.copyWith(
                          color: scheme.outline,
                        ),
                      ),
                    ),
                  ],
                ),
              ),
          ],
        ),
      ),
    );
  }
}

class _ProbeRow extends StatelessWidget {
  final ProbeEntry entry;
  final VoidCallback? onTap;

  const _ProbeRow({required this.entry, this.onTap});

  @override
  Widget build(BuildContext context) {
    final l10n = context.appLocalizations;
    final scheme = context.colorScheme;
    final textTheme = context.textTheme;
    final result = entry.result;
    // 颜色走迟滞后的档位（单次失败不改色），演示数据没有 health 就现推。
    final health = entry.health ??
        (result == null
            ? ProbeHealth.unknown
            : deriveProbeHealth(ProbeHealth.unknown, result));
    final _Tone tone = switch (health) {
      ProbeHealth.ok => _Tone.pass,
      ProbeHealth.degraded => _Tone.warn,
      ProbeHealth.down => _Tone.error,
      ProbeHealth.unknown => _Tone.neutral,
    };
    final String head;
    if (result == null) {
      head = '—';
    } else if (result.ok) {
      head = result.rttMs == null ? '✓' : '✓ ${result.rttMs} ms';
    } else {
      head = '✗ ${l10n.networkProbeTimeout}';
    }
    final rate = result == null
        ? '—'
        : l10n.networkProbeRate(result.successCount, result.windowCount);

    return InkWell(
      onTap: onTap,
      child: Padding(
        padding: const EdgeInsets.fromLTRB(16, 10, 16, 10),
        child: Row(
          children: [
            Expanded(
              child: Column(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: [
                  Text(entry.target.name, style: textTheme.bodyMedium),
                  Text(
                    entry.target.endpoint,
                    style: textTheme.bodySmall?.copyWith(color: scheme.outline),
                  ),
                ],
              ),
            ),
            Column(
              crossAxisAlignment: CrossAxisAlignment.end,
              children: [
                Text(
                  head,
                  style: textTheme.labelLarge?.copyWith(
                    color: _toneColor(context, tone),
                  ),
                ),
                Text(
                  rate,
                  style: textTheme.labelSmall?.copyWith(color: scheme.outline),
                ),
              ],
            ),
          ],
        ),
      ),
    );
  }
}

class _ProbeCard extends ConsumerWidget {
  final NetworkState state;

  const _ProbeCard({required this.state});

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final l10n = context.appLocalizations;
    final scheme = context.colorScheme;
    final textTheme = context.textTheme;
    final editContext = _ProbeEditContext(
      suggestions: state.ztNeighborIps,
      subnetPrefix: _subnetPrefix(state.ztIp),
      defaultPort: ref.read(networkStateProvider.notifier).lastProbePort,
    );
    if (state.probes.isEmpty) {
      return Card(
        margin: EdgeInsets.zero,
        child: Padding(
          padding: const EdgeInsets.fromLTRB(16, 14, 16, 6),
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              Text(l10n.networkNoProbeTargets, style: textTheme.bodyMedium),
              const SizedBox(height: 4),
              Text(
                l10n.networkProbeHint,
                style: textTheme.bodySmall?.copyWith(color: scheme.outline),
              ),
              Align(
                alignment: Alignment.centerLeft,
                child: TextButton.icon(
                  onPressed: () => _showProbeDialog(context, ref, edit: editContext),
                  icon: const Icon(Icons.add, size: 18),
                  label: Text(l10n.networkAddProbe),
                ),
              ),
            ],
          ),
        ),
      );
    }
    final checkedAgo = state.checkedAgoSec;
    return Card(
      margin: EdgeInsets.zero,
      child: Column(
        children: [
          for (var i = 0; i < state.probes.length; i++) ...[
            if (i > 0) const Divider(height: 1, indent: 16, endIndent: 16),
            _ProbeRow(
              entry: state.probes[i],
              onTap: () => _showProbeDialog(
                context,
                ref,
                target: state.probes[i].target,
                edit: editContext,
              ),
            ),
          ],
          const Divider(height: 1, indent: 16, endIndent: 16),
          Padding(
            padding: const EdgeInsets.fromLTRB(8, 0, 16, 0),
            child: Row(
              children: [
                TextButton.icon(
                  onPressed: () => _showProbeDialog(context, ref, edit: editContext),
                  icon: const Icon(Icons.add, size: 18),
                  label: Text(l10n.networkAddProbe),
                ),
                const Spacer(),
                Text(
                  checkedAgo == null
                      ? l10n.networkNotChecked
                      : l10n.networkCheckedAgo(checkedAgo),
                  style: textTheme.labelSmall?.copyWith(
                    color: scheme.outline,
                  ),
                ),
              ],
            ),
          ),
        ],
      ),
    );
  }
}

class _PeersCard extends StatelessWidget {
  final NetworkState state;

  const _PeersCard({required this.state});

  @override
  Widget build(BuildContext context) {
    final l10n = context.appLocalizations;
    final scheme = context.colorScheme;
    final textTheme = context.textTheme;
    return Card(
      margin: EdgeInsets.zero,
      child: ExpansionTile(
        title: Text(l10n.networkPeers),
        subtitle: Text(
          _peerSummary(context, state),
          style: textTheme.bodySmall?.copyWith(color: scheme.outline),
        ),
        childrenPadding: const EdgeInsets.fromLTRB(16, 0, 16, 12),
        children: [
          if (state.peers == null)
            const Align(
              alignment: Alignment.centerLeft,
              child: Text('—'),
            )
          else if (state.peers!.isEmpty)
            Align(
              alignment: Alignment.centerLeft,
              child: Text(
                l10n.networkPeersEmptyDetail,
                style: textTheme.bodySmall?.copyWith(color: scheme.outline),
              ),
            )
          else
            for (final peer in state.peers!)
              Padding(
                padding: const EdgeInsets.symmetric(vertical: 4),
                child: Row(
                  children: [
                    Expanded(
                      child: Column(
                        crossAxisAlignment: CrossAxisAlignment.start,
                        children: [
                          Text(
                            peer.address,
                            style: textTheme.bodySmall,
                          ),
                          if (_peerDetail(context, peer).isNotEmpty)
                            Text(
                              _peerDetail(context, peer),
                              style: textTheme.labelSmall?.copyWith(
                                color: scheme.outline,
                              ),
                            ),
                        ],
                      ),
                    ),
                    Text(
                      peer.latencyMs == null ? '—' : '${peer.latencyMs} ms',
                      style: textTheme.bodySmall,
                    ),
                    const SizedBox(width: 12),
                    Text(
                      switch (peer.path) {
                        PeerPath.direct => l10n.networkDirect,
                        PeerPath.relay => l10n.networkRelay,
                        PeerPath.unknown => '—',
                      },
                      style: textTheme.bodySmall?.copyWith(
                        color: peer.path == PeerPath.relay
                            ? _toneColor(context, _Tone.warn)
                            : scheme.outline,
                      ),
                    ),
                  ],
                ),
              ),
        ],
      ),
    );
  }
}

class _DiagnosticsCard extends StatelessWidget {
  final NetworkState state;

  const _DiagnosticsCard({required this.state});

  @override
  Widget build(BuildContext context) {
    final l10n = context.appLocalizations;
    return Card(
      margin: EdgeInsets.zero,
      child: ExpansionTile(
        title: Text(l10n.networkDiagnostics),
        childrenPadding: const EdgeInsets.fromLTRB(16, 0, 16, 12),
        children: [
          _kv(context, 'Network ID', state.networkId),
          _kv(context, 'Node ID', state.nodeAddress),
          _kv(
            context,
            l10n.networkManagedRoutes,
            l10n.networkRouteCount(state.routesCount),
          ),
          _kv(context, l10n.networkUdpPort, state.udpPort?.toString()),
          _kv(context, l10n.networkSocketGen, state.socketGen?.toString()),
          _kv(
            context,
            l10n.networkSelfHeal,
            state.selfHealCount == null
                ? null
                : '${state.selfHealCount}/${state.selfHealMax}',
          ),
          _kv(context, l10n.networkLastError, state.lastError),
          _kv(context, l10n.networkAppVersion, _appVersion()),
        ],
      ),
    );
  }

  Widget _kv(BuildContext context, String label, String? value) {
    final scheme = context.colorScheme;
    final textTheme = context.textTheme;
    final display = (value == null || value.isEmpty) ? '—' : value;
    return Padding(
      padding: const EdgeInsets.symmetric(vertical: 3),
      child: Row(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: [
          SizedBox(
            width: 104,
            child: Text(
              label,
              style: textTheme.bodySmall?.copyWith(color: scheme.outline),
            ),
          ),
          Expanded(
            child: Text(display, style: textTheme.bodySmall),
          ),
        ],
      ),
    );
  }
}

/// 调试菜单：默认「真实数据」，其余项强制覆盖成演示场景（真机排查用）。
class _DemoSceneButton extends ConsumerWidget {
  const _DemoSceneButton();

  static const _liveValue = 'live';

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final l10n = context.appLocalizations;
    final scene = ref.watch(networkDemoSceneProvider);
    return PopupMenuButton<String>(
      icon: const Icon(Icons.tune),
      tooltip: l10n.networkPrototypeSwitch,
      initialValue: scene?.name ?? _liveValue,
      onSelected: (value) => ref.read(networkDemoSceneProvider.notifier).select(
            value == _liveValue ? null : NetworkDemoScene.values.byName(value),
          ),
      itemBuilder: (_) => [
        PopupMenuItem<String>(
          value: _liveValue,
          child: Text(l10n.networkLiveData),
        ),
        for (final value in NetworkDemoScene.values)
          PopupMenuItem<String>(
            value: value.name,
            child: Text(value.label),
          ),
      ],
    );
  }
}

// ---------------------------------------------------------------------------
// 探测目标编辑对话框（第一版只接受 IP 字面量，不做 DNS）
// ---------------------------------------------------------------------------

class _ProbeEditContext {
  final List<String> suggestions;
  final String? subnetPrefix;
  final int? defaultPort;

  const _ProbeEditContext({
    this.suggestions = const [],
    this.subnetPrefix,
    this.defaultPort,
  });
}

/// 从当前 ZT 分配地址推导 /24 前缀（仅 IPv4）：新建目标时预填，少打三节。
String? _subnetPrefix(String? ip) {
  if (ip == null) return null;
  final parts = ip.split('.');
  if (parts.length != 4) return null;
  return '${parts[0]}.${parts[1]}.${parts[2]}.';
}

/// 当前 App 版本（PackageInfo；初始化未完成时返回 null）。
String? _appVersion() {
  try {
    final info = globalState.packageInfo;
    return info.buildNumber.isEmpty
        ? info.version
        : '${info.version}+${info.buildNumber}';
  } catch (_) {
    return null;
  }
}

class _ProbeDialogResult {
  final ProbeTarget? target;
  final bool delete;

  const _ProbeDialogResult.save(this.target) : delete = false;
  const _ProbeDialogResult.delete()
      : target = null,
        delete = true;
}

Future<void> _showProbeDialog(
  BuildContext context,
  WidgetRef ref, {
  ProbeTarget? target,
  _ProbeEditContext? edit,
}) async {
  final result = await showDialog<_ProbeDialogResult>(
    context: context,
    builder: (_) => _ProbeDialog(
      target: target,
      edit: edit ?? const _ProbeEditContext(),
    ),
  );
  if (result == null) return;
  final notifier = ref.read(networkStateProvider.notifier);
  if (result.delete) {
    if (target != null) await notifier.removeProbe(target.id);
    return;
  }
  final saved = result.target;
  if (saved != null) await notifier.saveProbe(saved);
}

class _ProbeDialog extends StatefulWidget {
  final ProbeTarget? target;
  final _ProbeEditContext edit;

  const _ProbeDialog({this.target, this.edit = const _ProbeEditContext()});

  @override
  State<_ProbeDialog> createState() => _ProbeDialogState();
}

class _ProbeDialogState extends State<_ProbeDialog> {
  late final TextEditingController _name;
  late final TextEditingController _host;
  late final TextEditingController _port;
  String? _error;

  @override
  void initState() {
    super.initState();
    _name = TextEditingController(text: widget.target?.name ?? '');
    _host = TextEditingController(
      text: widget.target?.host ?? widget.edit.subnetPrefix ?? '',
    );
    _port = TextEditingController(
      text: widget.target?.port.toString() ??
          widget.edit.defaultPort?.toString() ??
          '',
    );
  }

  @override
  void dispose() {
    _name.dispose();
    _host.dispose();
    _port.dispose();
    super.dispose();
  }

  void _save() {
    final l10n = context.appLocalizations;
    final host = _host.text.trim();
    final port = int.tryParse(_port.text.trim()) ?? 0;
    if (!isValidProbeHost(host)) {
      setState(() => _error = l10n.networkProbeIpOnly);
      return;
    }
    if (port <= 0 || port > 65535) {
      setState(() => _error = l10n.networkProbeInvalidPort);
      return;
    }
    final name = _name.text.trim().isEmpty ? host : _name.text.trim();
    final id = widget.target?.id ??
        DateTime.now().microsecondsSinceEpoch.toString();
    Navigator.of(context).pop(
      _ProbeDialogResult.save(
        ProbeTarget(
          id: id,
          name: name,
          host: host,
          port: port,
          enabled: widget.target?.enabled ?? true,
        ),
      ),
    );
  }

  @override
  Widget build(BuildContext context) {
    final l10n = context.appLocalizations;
    final suggestions = widget.edit.suggestions;
    return AlertDialog(
      title: Text(widget.target == null ? l10n.networkAddProbe : l10n.edit),
      content: SingleChildScrollView(
        child: Column(
          mainAxisSize: MainAxisSize.min,
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            TextField(
              controller: _name,
              decoration: InputDecoration(
                labelText: l10n.name,
                isDense: true,
              ),
            ),
            const SizedBox(height: 8),
            TextField(
              controller: _host,
              keyboardType: TextInputType.url,
              decoration: InputDecoration(
                labelText: l10n.host,
                helperText: l10n.networkProbeIpOnly,
                isDense: true,
              ),
            ),
            if (suggestions.isNotEmpty)
              Padding(
                padding: const EdgeInsets.only(top: 8),
                child: Wrap(
                  spacing: 6,
                  runSpacing: 4,
                  children: [
                    for (final ip in suggestions.take(8))
                      ActionChip(
                        label: Text(ip, style: context.textTheme.labelSmall),
                        onPressed: () => setState(() {
                          _host.text = ip;
                          _host.selection = TextSelection.collapsed(
                            offset: ip.length,
                          );
                        }),
                      ),
                  ],
                ),
              ),
            const SizedBox(height: 8),
            TextField(
              controller: _port,
              keyboardType: TextInputType.number,
              decoration: InputDecoration(
                labelText: l10n.port,
                isDense: true,
              ),
            ),
            if (_error != null)
              Padding(
                padding: const EdgeInsets.only(top: 8),
                child: Text(
                  _error!,
                  style: context.textTheme.bodySmall?.copyWith(
                    color: context.colorScheme.error,
                  ),
                ),
              ),
          ],
        ),
      ),
      actions: [
        TextButton(
          onPressed: () => Navigator.of(context).pop(),
          child: Text(l10n.cancel),
        ),
        if (widget.target != null)
          TextButton(
            onPressed: () => Navigator.of(context).pop(
              const _ProbeDialogResult.delete(),
            ),
            child: Text(l10n.delete),
          ),
        FilledButton(
          onPressed: _save,
          child: Text(l10n.save),
        ),
      ],
    );
  }
}
