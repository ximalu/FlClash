import 'dart:io';

import 'package:fl_clash/common/common.dart';
import 'package:fl_clash/providers/providers.dart';
import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

class ZeroTierStatus extends ConsumerWidget {
  const ZeroTierStatus({super.key});

  Future<bool> _enabled() async {
    final home = await appPath.homeDirPath;
    final file = File('$home/zerotier.json');
    if (!await file.exists()) return false;
    try {
      final text = await file.readAsString();
      return RegExp(r'"network-id"\s*:\s*"[^"\\s]+"').hasMatch(text);
    } catch (_) {
      return false;
    }
  }

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final logs = ref.watch(logsProvider).list.reversed;
    final ztLogs = logs.where((log) => log.payload.contains('[ZT]')).toList();

    String status = 'Unknown';
    String detail = 'No ZeroTier runtime event';
    IconData icon = Icons.help_outline;
    Color color = context.colorScheme.outline;

    for (final log in ztLogs) {
      final text = log.payload;
      if (text.contains('engine RUNNING')) {
        status = 'Running';
        icon = Icons.check_circle_outline;
        color = context.colorScheme.primary;
        final route = RegExp(r'routes=(\d+)').firstMatch(text);
        detail = route == null ? 'Engine running' : 'Managed routes: ${route.group(1)}';
        break;
      }
      if (text.contains('engine STOPPED')) {
        status = 'Stopped';
        icon = Icons.pause_circle_outline;
        color = context.colorScheme.outline;
        detail = 'Engine stopped';
        break;
      }
      if (text.contains('engine start failed')) {
        status = 'Error';
        icon = Icons.error_outline;
        color = context.colorScheme.error;
        detail = text.replaceFirst(RegExp(r'^.*?\[ZT\]\s*'), '');
        break;
      }
      if (text.contains('UDP socket bound') || text.contains('join 0x')) {
        status = 'Starting';
        icon = Icons.sync;
        color = context.colorScheme.tertiary;
        detail = 'Engine starting';
      }
    }

    return FutureBuilder<bool>(
      future: _enabled(),
      builder: (context, snapshot) {
        if (snapshot.connectionState == ConnectionState.done && snapshot.data != true) {
          status = 'Disabled';
          detail = 'ZeroTier is not configured';
          icon = Icons.power_off_outlined;
          color = context.colorScheme.outline;
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
      },
    );
  }
}
