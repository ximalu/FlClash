import 'dart:async';
import 'dart:convert';
import 'dart:io';

import 'package:fl_clash/common/common.dart';
import 'package:fl_clash/widgets/widgets.dart';
import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:path/path.dart' as p;

/// FlClashTier: ZeroTier 网络设置页（独立栏目）。
///
/// 写入 `HomeDir/zerotier.json`（Go core 在 TUN 启动时读取；改动后重启 VPN 生效）。
/// 清空 network-id = 禁用 ZeroTier（纯 mihomo 模式，与 M0 行为一致）。
class ZeroTierView extends ConsumerStatefulWidget {
  const ZeroTierView({super.key});

  @override
  ConsumerState<ZeroTierView> createState() => _ZeroTierViewState();
}

class _ZeroTierViewState extends ConsumerState<ZeroTierView> {
  final _controller = TextEditingController();
  Timer? _debounce;
  String _status = '';

  @override
  void initState() {
    super.initState();
    _load();
  }

  @override
  void dispose() {
    _debounce?.cancel();
    _controller.dispose();
    super.dispose();
  }

  Future<File> _configFile() async {
    final dir = await appPath.homeDirPath;
    return File(p.join(dir, 'zerotier.json'));
  }

  Future<void> _load() async {
    var nwid = '';
    try {
      final file = await _configFile();
      if (await file.exists()) {
        final json = jsonDecode(await file.readAsString());
        nwid = ((json as Map<String, dynamic>)['network-id'] as String?) ?? '';
      }
    } catch (_) {
      // 文件缺失/损坏 → 视为未配置
    }
    _controller.text = nwid.trim();
    _status = nwid.trim().isEmpty
        ? 'ZeroTier disabled (mihomo only)'
        : 'ZeroTier network: ${nwid.trim()}';
    if (mounted) setState(() {});
  }

  Future<void> _save(String value) async {
    final nwid = value.trim();
    try {
      final file = await _configFile();
      if (nwid.isEmpty) {
        if (await file.exists()) await file.delete();
        _status = 'ZeroTier disabled (mihomo only)';
      } else {
        await file.writeAsString('{"network-id": "$nwid"}\n');
        _status = 'saved: $nwid — restart VPN to apply';
      }
      if (mounted) setState(() {});
    } catch (err) {
      _status = 'save failed: $err';
      if (mounted) setState(() {});
    }
  }

  @override
  Widget build(BuildContext context) {
    final theme = Theme.of(context);
    return BaseScaffold(
      title: 'ZeroTier',
      body: ListView(
        padding: const EdgeInsets.only(bottom: 32),
        children: [
          Padding(
            padding: const EdgeInsets.fromLTRB(16, 16, 16, 8),
            child: Text(
              'ZeroTier 网络互联：配置 Network ID 后，命中 ZeroTier '
              'Managed Routes 的流量走 ZeroTier 内网，其余流量走 mihomo。',
              style: theme.textTheme.bodySmall,
            ),
          ),
          ListTile(
            title: const Text('ZeroTier Network ID'),
            subtitle: Column(
              crossAxisAlignment: CrossAxisAlignment.start,
              children: [
                const SizedBox(height: 4),
                TextField(
                  controller: _controller,
                  decoration: const InputDecoration(
                    hintText: 'e.g. b6079f73c6c0eb31 (empty = disabled)',
                    isDense: true,
                    border: OutlineInputBorder(),
                  ),
                  style: theme.textTheme.bodyMedium,
                  onChanged: (value) {
                    _debounce?.cancel();
                    _debounce = Timer(
                      const Duration(milliseconds: 800),
                      () => _save(value),
                    );
                  },
                ),
                if (_status.isNotEmpty) ...[
                  const SizedBox(height: 4),
                  Text(
                    _status,
                    style: theme.textTheme.bodySmall,
                  ),
                ],
              ],
            ),
          ),
          const Divider(height: 0),
          Padding(
            padding: const EdgeInsets.fromLTRB(16, 16, 16, 8),
            child: Text(
              '使用步骤',
              style: theme.textTheme.titleSmall,
            ),
          ),
          const _StepText(
            '1. 先在「配置文件」页导入 Clash 配置（订阅链接/文件），'
            '此时首页才会出现启动 VPN 的按钮。',
          ),
          const _StepText(
            '2. 在本页填写 ZeroTier Network ID（可在 ZeroTier Central '
            '创建网络后获得）。',
          ),
          const _StepText(
            '3. 启动 VPN（首次会弹出系统授权），然后在 ZeroTier Central '
            '授权本机节点。',
          ),
          const _StepText(
            '4. 修改 Network ID 后需重启 VPN 才生效；清空后为纯 mihomo 模式。',
          ),
        ],
      ),
    );
  }
}

class _StepText extends StatelessWidget {
  const _StepText(this.text);

  final String text;

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.symmetric(horizontal: 16, vertical: 4),
      child: Text(
        text,
        style: Theme.of(context).textTheme.bodySmall,
      ),
    );
  }
}
