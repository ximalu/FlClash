import 'dart:async';
import 'dart:convert';
import 'dart:io';

import 'package:fl_clash/common/common.dart';
import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

/// Dashboard card showing the current ZeroTier runtime state.
///
/// The status file is a runtime heartbeat, not persistent state. A stale
/// RUNNING record is treated as STOPPED/Unknown after the heartbeat timeout,
/// which prevents a previous process's state surviving an Android reboot.
///
/// If `zerotier.json` has no network-id the card shows Disabled.
class ZeroTierStatus extends ConsumerStatefulWidget {
  const ZeroTierStatus({super.key});

  @override
  ConsumerState<ZeroTierStatus> createState() => _ZeroTierStatusState();
}

class _ZeroTierStatusState extends ConsumerState<ZeroTierStatus> {
  static final _networkIdRegExp = RegExp(r'"network-id"\s*:\s*"[^"\s]+"');
  static const _heartbeatTimeout = Duration(seconds: 10);

  Timer? _timer;
  bool _configured = false;
  String _state = 'UNKNOWN';
  String _ipv4 = '';
  int _routes = 0;
  String _error = '';

  @override
  void initState() {
    super.initState();
    _refresh();
    _timer = Timer.periodic(const Duration(seconds: 2), (_) => _refresh());
  }

  @override
  void dispose() {
    _timer?.cancel();
    super.dispose();
  }

  Future<void> _refresh() async {
    final home = await appPath.homeDirPath;
    bool configured = false;
    String state = 'UNKNOWN';
    String ipv4 = '';
    int routes = 0;
    String error = '';
    try {
      final cfgFile = File('$home/zerotier.json');
      if (await cfgFile.exists()) {
        final text = await cfgFile.readAsString();
        configured = _networkIdRegExp.hasMatch(text);
      }
      final statusFile = File('$home/zerotier-status.json');
      if (await statusFile.exists()) {
        final decoded = jsonDecode(await statusFile.readAsString());
        if (decoded is Map<String, dynamic>) {
          state = (decoded['state'] as String?) ?? 'UNKNOWN';
          ipv4 = (decoded['ipv4'] as String?) ?? '';
          routes = (decoded['routes'] as num?)?.toInt() ?? 0;
          error = (decoded['error'] as String?) ?? '';

          // RUNNING is valid only while the engine is actively refreshing
          // this heartbeat. A status file left by a process that died (for
          // example during an Android reboot) is stale and must not report OK.
          if (state == 'RUNNING') {
            final updatedAt = (decoded['updatedAt'] as num?)?.toInt();
            final now = DateTime.now().millisecondsSinceEpoch;
            if (updatedAt == null || now - updatedAt > _heartbeatTimeout.inMilliseconds) {
              state = 'STOPPED';
              ipv4 = '';
              routes = 0;
              error = '';
            }
          }
        }
      }
    } catch (_) {
      // 读取/解析失败保持 Unknown，下个 tick 重试
    }
    if (!mounted) return;
    setState(() {
      _configured = configured;
      _state = state;
      _ipv4 = ipv4;
      _routes = routes;
      _error = error;
    });
  }

  @override
  Widget build(BuildContext context) {
    final colorScheme = context.colorScheme;
    String status;
    String detail;
    IconData icon;
    Color color;

    if (!_configured) {
      status = 'Disabled';
      detail = 'ZeroTier is not configured';
      icon = Icons.power_off_outlined;
      color = colorScheme.outline;
    } else if (_state == 'RUNNING') {
      if (_ipv4.isNotEmpty) {
        status = 'OK';
        detail = '$_ipv4 · $_routes route${_routes == 1 ? '' : 's'}';
        icon = Icons.check_circle_outline;
        color = colorScheme.primary;
      } else {
        status = 'Running';
        detail = 'Engine running, waiting for IP';
        icon = Icons.sync;
        color = colorScheme.tertiary;
      }
    } else if (_state == 'ERROR') {
      status = 'Error';
      detail = _error.isEmpty ? 'Engine start failed' : _error;
      icon = Icons.error_outline;
      color = colorScheme.error;
    } else if (_state == 'STOPPED') {
      status = 'Stopped';
      detail = 'Engine stopped';
      icon = Icons.pause_circle_outline;
      color = colorScheme.outline;
    } else {
      status = 'Unknown';
      detail = 'Engine not started';
      icon = Icons.help_outline;
      color = colorScheme.outline;
    }

    return Card(
      margin: EdgeInsets.zero,
      child: ListTile(
        dense: true,
        leading: Icon(icon, color: color),
        title: const Text('ZeroTier'),
        subtitle: Text(detail, maxLines: 1, overflow: TextOverflow.ellipsis),
        trailing: Text(
          status,
          style: context.textTheme.labelLarge?.copyWith(color: color),
        ),
      ),
    );
  }
}
