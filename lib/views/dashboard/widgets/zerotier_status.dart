import 'dart:async';
import 'dart:io';

import 'package:fl_clash/common/common.dart';
import 'package:fl_clash/core/core.dart';
import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

/// Dashboard card showing the current ZeroTier runtime state.
///
/// Runtime state comes from the live Go Engine through FlClash's existing
/// Core RPC path. The persistent zerotier-status.json file is intentionally
/// not used for liveness; it remains a diagnostic snapshot only.
class ZeroTierStatus extends ConsumerStatefulWidget {
  const ZeroTierStatus({super.key});

  @override
  ConsumerState<ZeroTierStatus> createState() => _ZeroTierStatusState();
}

class _ZeroTierStatusState extends ConsumerState<ZeroTierStatus> {
  static final _networkIdRegExp = RegExp(r'"network-id"\s*:\s*"[^"\s]+"');

  Timer? _timer;
  bool _configured = false;
  String _state = 'UNKNOWN';
  String _ipv4 = '';
  int _routes = 0;

  @override
  void initState() {
    super.initState();
    _refresh();
    // This polls the in-memory Engine through Core RPC; it performs no disk
    // I/O for runtime state and never treats a persisted value as liveness.
    _timer = Timer.periodic(const Duration(seconds: 2), (_) => _refresh());
  }

  @override
  void dispose() {
    _timer?.cancel();
    super.dispose();
  }

  Future<void> _refresh() async {
    bool configured = false;
    try {
      final home = await appPath.homeDirPath;
      final cfgFile = File('$home/zerotier.json');
      if (await cfgFile.exists()) {
        final text = await cfgFile.readAsString();
        configured = _networkIdRegExp.hasMatch(text);
      }
    } catch (_) {}

    String state = 'STOPPED';
    String ipv4 = '';
    int routes = 0;
    try {
      final data = await coreController.getZeroTierStatus();
      state = data['state'] as String? ?? 'STOPPED';
      ipv4 = data['ipv4'] as String? ?? '';
      routes = (data['routes'] as num?)?.toInt() ?? 0;
    } catch (_) {
      // If Core RPC is unavailable, the in-process ZeroTier Engine cannot be
      // queried. Treat it as not running rather than showing stale state.
    }

    if (!mounted) return;
    setState(() {
      _configured = configured;
      _state = state;
      _ipv4 = ipv4;
      _routes = routes;
    });
  }

  @override
  Widget build(BuildContext context) {
    final colorScheme = context.colorScheme;
    final appLocalizations = context.appLocalizations;
    String status;
    String detail;
    IconData icon;
    Color color;

    if (!_configured) {
      status = appLocalizations.zeroTierDisabled;
      detail = appLocalizations.zeroTierNotConfigured;
      icon = Icons.power_off_outlined;
      color = colorScheme.outline;
    } else if (_state == 'RUNNING') {
      if (_ipv4.isNotEmpty) {
        status = appLocalizations.zeroTierOk;
        detail = '$_ipv4 · ${appLocalizations.zeroTierRoutesCount(_routes)}';
        icon = Icons.check_circle_outline;
        color = colorScheme.primary;
      } else {
        status = appLocalizations.zeroTierRunning;
        detail = appLocalizations.zeroTierWaitingIp;
        icon = Icons.sync;
        color = colorScheme.tertiary;
      }
    } else if (_state == 'STARTING') {
      status = appLocalizations.zeroTierStarting;
      detail = appLocalizations.zeroTierStartingDetail;
      icon = Icons.sync;
      color = colorScheme.tertiary;
    } else if (_state == 'STOPPING') {
      status = appLocalizations.zeroTierStopping;
      detail = appLocalizations.zeroTierStoppingDetail;
      icon = Icons.sync;
      color = colorScheme.tertiary;
    } else {
      status = appLocalizations.zeroTierStopped;
      detail = appLocalizations.zeroTierStoppedDetail;
      icon = Icons.pause_circle_outline;
      color = colorScheme.outline;
    }

    return Card(
      margin: EdgeInsets.zero,
      child: ListTile(
        dense: true,
        leading: Icon(icon, color: color),
        title: Text(appLocalizations.zeroTier),
        subtitle: Text(detail, maxLines: 1, overflow: TextOverflow.ellipsis),
        trailing: Text(
          status,
          style: context.textTheme.labelLarge?.copyWith(color: color),
        ),
      ),
    );
  }
}
