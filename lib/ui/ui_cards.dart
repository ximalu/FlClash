/// UI 卡片注册表 + 页面渲染器（schema v1）
///
/// 边界（用户 2026-11-26）：配置文件只能**引用**这里注册的 id，不能创造界面。
/// 新增一张卡 = 写 Dart + 在这里登记；改布局/顺序/分组/跨列 = 改 ui.json。
/// 详细动机：kb/projects/flclashtier/ui-schema-hotreload.md
library;

import 'package:fl_clash/common/common.dart';
import 'package:fl_clash/views/dashboard/widgets/widgets.dart';
import 'package:fl_clash/views/network.dart';
import 'package:fl_clash/widgets/widgets.dart';
import 'package:flutter/material.dart';

import 'ui_schema.dart';

typedef UiCardBuilder = Widget Function();

/// id → 卡片。
///
/// 命名约定：Dashboard 用原 `DashboardWidget` 枚举名（networkSpeed…），
/// 网络页用页面语义（networkService / exitMihomo / exitZerotier / …）。
final Map<String, UiCardBuilder> uiCardRegistry = {
  // ---- Dashboard ----
  'networkSpeed': () => const NetworkSpeed(),
  'outboundModeV2': () => const OutboundModeV2(),
  'outboundMode': () => const OutboundMode(),
  'trafficUsage': () => const TrafficUsage(),
  'networkDetection': () => const NetworkDetection(),
  'outboundSummary': () => const OutboundSummary(),
  'vpnButton': () => const VpnButton(),
  'tunButton': () => const TUNButton(),
  'systemProxyButton': () => const SystemProxyButton(),
  'intranetIp': () => const IntranetIP(),
  'memoryInfo': () => const MemoryInfo(),

  // ---- 网络页 ----
  'networkService': () => const NetworkServiceCard(),
  'exitMihomo': () => const NetworkExitCard(zerotier: false),
  'exitZerotier': () => const NetworkExitCard(zerotier: true),
  'networkProbes': () => const NetworkProbesCard(),
  'networkPeers': () => const NetworkPeersCard(),
  'networkDiagnostics': () => const NetworkDiagnosticsCard(),
};

Set<String> get uiKnownCardIds => uiCardRegistry.keys.toSet();

/// 内置布局与 schema 的唯一真相源：schema 有该页就用 schema，否则返回 null
/// （调用方回落原来的手写布局）。
Widget? buildUiPage(
  BuildContext context,
  UiSchema? schema,
  String pageId, {
  EdgeInsets padding = const EdgeInsets.fromLTRB(12, 8, 12, 24),
}) {
  final page = schema?.page(pageId);
  if (page == null) {
    return null;
  }
  return UiPageBody(page: page, padding: padding);
}

class UiPageBody extends StatelessWidget {
  final UiPageSpec page;
  final EdgeInsets padding;

  const UiPageBody({super.key, required this.page, this.padding = EdgeInsets.zero});

  @override
  Widget build(BuildContext context) {
    final isGrid = page.layout == 'grid';
    final gap = isGrid ? 14.0 : 8.0;
    final sections = <Widget>[];
    for (var i = 0; i < page.sections.length; i++) {
      final section = page.sections[i];
      final title = _sectionTitle(context, section.title);
      if (i > 0) {
        sections.add(SizedBox(height: isGrid ? gap : 16));
      }
      if (title != null) {
        sections.add(_UiSectionTitle(title));
      }
      if (isGrid) {
        sections.add(
          Grid(
            crossAxisCount: page.columns,
            crossAxisSpacing: gap,
            mainAxisSpacing: gap,
            children: [
              for (final card in section.cards)
                if (uiCardRegistry[card.id] != null)
                  GridItem(
                    crossAxisCellCount: card.span,
                    child: uiCardRegistry[card.id]!(),
                  ),
            ],
          ),
        );
      } else {
        for (final card in section.cards) {
          final builder = uiCardRegistry[card.id];
          if (builder == null) {
            continue;
          }
          sections.add(builder());
          sections.add(SizedBox(height: gap));
        }
      }
    }
    final content = Padding(
      padding: padding,
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: sections,
      ),
    );
    // 页面级 textScale 已废弃（2026-11-27 定案 A）：字号完全跟随主题/系统字号，
    // 一套机制、无障碍字号全局生效。schema 仍解析该字段（旧 ui.json 不报错），但不再生效。
    return content;
  }

  /// 分区标题只认 l10n key，不认字面量——避免配置里出现需要翻译的文案。
  String? _sectionTitle(BuildContext context, String? key) {
    if (key == null) {
      return null;
    }
    final l10n = context.appLocalizations;
    return switch (key) {
      'networkVpnService' => l10n.networkVpnService,
      'networkExits' => l10n.networkExits,
      'networkProbes' => l10n.networkProbes,
      _ => null,
    };
  }
}

class _UiSectionTitle extends StatelessWidget {
  final String text;

  const _UiSectionTitle(this.text);

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
