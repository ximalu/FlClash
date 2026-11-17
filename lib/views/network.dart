import 'package:fl_clash/common/common.dart';
import 'package:fl_clash/models/network_state.dart';
import 'package:fl_clash/providers/network.dart';
import 'package:fl_clash/widgets/widgets.dart';
import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

/// 静态原型：数据全部来自 providers/network.dart 的演示实现。
///
/// 界面规范：kb/projects/flclashtier/network-ui-states.md
/// 状态模型：kb/projects/flclashtier/network-state-model.md
///
/// 本页只消费 NetworkState，不读 core（原则 P4）。
/// TODO(l10n): 原型阶段文案为中文硬编码，冻结 UI 后统一迁入 arb。
class NetworkView extends ConsumerWidget {
  const NetworkView({super.key});

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final state = ref.watch(networkStateProvider);
    final uiState = deriveUiState(state);
    return CommonScaffold(
      title: '网络',
      actions: [
        IconButton(
          tooltip: '重测',
          icon: const Icon(Icons.refresh),
          onPressed:
              uiState == NetworkUiState.s0VpnOff ? null : () => _notWired(context),
        ),
        const _DemoSceneButton(),
      ],
      body: ListView(
        padding: const EdgeInsets.fromLTRB(12, 8, 12, 24),
        children: [
          Padding(
            padding: const EdgeInsets.only(bottom: 12, left: 4),
            child: Text(
              '静态原型 · 数据为模拟，用于确认布局与状态语义',
              style: context.textTheme.labelSmall?.copyWith(
                color: context.colorScheme.outline,
              ),
            ),
          ),
          const _SectionTitle('VPN 服务'),
          _ServiceCard(state: state),
          const SizedBox(height: 24),
          const _SectionTitle('出口'),
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
          const _SectionTitle('连通性探测'),
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
    const SnackBar(
      content: Text('静态原型：此操作尚未接入'),
      duration: Duration(seconds: 2),
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
    final on = state.vpn == VpnStatus.on;
    final subtitle = on
        ? '已连接 · ${state.enabledExitCount} 个出口启用'
        : '未运行 · 开启后按保存的偏好启动出口';
    final scheme = context.colorScheme;
    return Card(
      margin: EdgeInsets.zero,
      child: ListTile(
        title: const Text('VPN 服务'),
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
  required ExitState exit,
  required NetworkUiState uiState,
  required bool isZeroTier,
}) {
  if (uiState == NetworkUiState.s0VpnOff) {
    return const _ExitBadgeData('已停止', _Tone.neutral);
  }
  switch (exit.status) {
    case ExitRunStatus.error:
      return const _ExitBadgeData('启动失败', _Tone.error);
    case ExitRunStatus.notEnabled:
      return const _ExitBadgeData('未启用', _Tone.warn);
    case ExitRunStatus.starting:
      return const _ExitBadgeData('启动中', _Tone.warn);
    case ExitRunStatus.stopped:
      return const _ExitBadgeData('已停止', _Tone.neutral);
    case ExitRunStatus.running:
      if (!isZeroTier) {
        return const _ExitBadgeData('运行中', _Tone.pass);
      }
      if (uiState == NetworkUiState.s2LinkStalled) {
        return const _ExitBadgeData('链路异常', _Tone.error);
      }
      if (uiState == NetworkUiState.s3WaitingConfig) {
        return const _ExitBadgeData('等待配置', _Tone.warn);
      }
      return const _ExitBadgeData('直连', _Tone.pass);
  }
}

String _peerSummary(NetworkState state) {
  if (state.peers.isEmpty) {
    return '暂无对端';
  }
  return '${state.peers.length} 个对端 · ${state.directPeers} 直连 · ${state.relayPeers} 中继';
}

String? _disabledReason({
  required ExitState exit,
  required NetworkUiState uiState,
  required bool isZeroTier,
}) {
  final vpnOff = uiState == NetworkUiState.s0VpnOff;
  if (exit.switchDisabledReason != null) {
    return vpnOff && !isZeroTier
        ? '${exit.switchDisabledReason} · VPN 未运行'
        : exit.switchDisabledReason;
  }
  return vpnOff ? 'VPN 未运行' : null;
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
    final scheme = context.colorScheme;
    final textTheme = context.textTheme;
    final badge = _badgeFor(
      exit: exit,
      uiState: uiState,
      isZeroTier: isZeroTier,
    );
    final disabledReason = _disabledReason(
      exit: exit,
      uiState: uiState,
      isZeroTier: isZeroTier,
    );
    final vpnOff = uiState == NetworkUiState.s0VpnOff;
    final showPeerSummary =
        isZeroTier && exit.status == ExitRunStatus.running && !vpnOff;

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
                              exit.name,
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
                  '偏好已保留，开启 VPN 后生效',
                  style: textTheme.bodySmall?.copyWith(color: scheme.outline),
                ),
              ),
            if (showPeerSummary)
              Padding(
                padding: const EdgeInsets.only(top: 6, right: 8),
                child: Text(
                  _peerSummary(state),
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

  const _ProbeRow({required this.entry});

  @override
  Widget build(BuildContext context) {
    final scheme = context.colorScheme;
    final textTheme = context.textTheme;
    final result = entry.result;
    final _Tone tone = result == null
        ? _Tone.neutral
        : (result.ok ? _Tone.pass : _Tone.error);
    final String head;
    if (result == null) {
      head = '—';
    } else if (result.ok) {
      head = result.rttMs == null ? '✓ 成功' : '✓ ${result.rttMs} ms';
    } else {
      head = '✗ 超时';
    }
    final rate = result == null
        ? '—'
        : '成功 ${result.successCount}/${result.windowCount}';

    return Padding(
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
    );
  }
}

class _ProbeCard extends StatelessWidget {
  final NetworkState state;

  const _ProbeCard({required this.state});

  @override
  Widget build(BuildContext context) {
    final scheme = context.colorScheme;
    final textTheme = context.textTheme;
    if (state.probes.isEmpty) {
      return Card(
        margin: EdgeInsets.zero,
        child: Padding(
          padding: const EdgeInsets.fromLTRB(16, 14, 16, 6),
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              Text('还没有探测目标。', style: textTheme.bodyMedium),
              const SizedBox(height: 4),
              Text(
                '添加一个地址后，这里会显示"从我这里能不能连通"。',
                style: textTheme.bodySmall?.copyWith(color: scheme.outline),
              ),
              Align(
                alignment: Alignment.centerLeft,
                child: TextButton.icon(
                  onPressed: () => _notWired(context),
                  icon: const Icon(Icons.add, size: 18),
                  label: const Text('添加探测目标'),
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
            _ProbeRow(entry: state.probes[i]),
          ],
          const Divider(height: 1, indent: 16, endIndent: 16),
          Padding(
            padding: const EdgeInsets.fromLTRB(8, 0, 16, 0),
            child: Row(
              children: [
                TextButton.icon(
                  onPressed: () => _notWired(context),
                  icon: const Icon(Icons.add, size: 18),
                  label: const Text('添加探测目标'),
                ),
                const Spacer(),
                Text(
                  checkedAgo == null ? '未检测' : '$checkedAgo 秒前检测',
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
    final scheme = context.colorScheme;
    final textTheme = context.textTheme;
    return Card(
      margin: EdgeInsets.zero,
      child: ExpansionTile(
        title: const Text('对端连接'),
        subtitle: Text(
          _peerSummary(state),
          style: textTheme.bodySmall?.copyWith(color: scheme.outline),
        ),
        childrenPadding: const EdgeInsets.fromLTRB(16, 0, 16, 12),
        children: [
          if (state.peers.isEmpty)
            Align(
              alignment: Alignment.centerLeft,
              child: Text(
                '暂无对端（ZeroTier 对端为懒建立，无流量时无记录）',
                style: textTheme.bodySmall?.copyWith(color: scheme.outline),
              ),
            )
          else
            for (final peer in state.peers)
              Padding(
                padding: const EdgeInsets.symmetric(vertical: 4),
                child: Row(
                  children: [
                    Expanded(
                      child: Text(
                        peer.address,
                        style: textTheme.bodySmall?.copyWith(
                          fontFeatures: const [FontFeature.tabularFigures()],
                        ),
                      ),
                    ),
                    Text(
                      peer.latencyMs == null ? '—' : '${peer.latencyMs} ms',
                      style: textTheme.bodySmall,
                    ),
                    const SizedBox(width: 12),
                    Text(
                      switch (peer.path) {
                        PeerPath.direct => '直连',
                        PeerPath.relay => '中继',
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
    return Card(
      margin: EdgeInsets.zero,
      child: ExpansionTile(
        title: const Text('诊断详情'),
        childrenPadding: const EdgeInsets.fromLTRB(16, 0, 16, 12),
        children: [
          _kv(context, 'Network ID', state.networkId),
          _kv(context, 'Node ID', state.nodeAddress),
          _kv(context, '管理路由', '${state.routesCount} 条'),
          _kv(context, 'UDP 端口', state.udpPort?.toString()),
          _kv(context, 'socket 世代', state.socketGen?.toString()),
          _kv(
            context,
            '自愈次数',
            '${state.selfHealCount}/${state.selfHealMax}',
          ),
          _kv(context, '最近错误', state.lastError),
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

class _DemoSceneButton extends ConsumerWidget {
  const _DemoSceneButton();

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final scene = ref.watch(networkDemoSceneProvider);
    return PopupMenuButton<NetworkDemoScene>(
      icon: const Icon(Icons.tune),
      tooltip: '调试：切换原型状态',
      initialValue: scene,
      onSelected: (value) =>
          ref.read(networkDemoSceneProvider.notifier).select(value),
      itemBuilder: (_) => [
        for (final value in NetworkDemoScene.values)
          PopupMenuItem<NetworkDemoScene>(
            value: value,
            child: Text(value.label),
          ),
      ],
    );
  }
}
