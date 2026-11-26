import 'package:fl_clash/common/common.dart';
import 'package:fl_clash/core/core.dart';
import 'package:fl_clash/providers/ui_schema.dart';
import 'package:fl_clash/ui/ui_cards.dart';
import 'package:fl_clash/ui/ui_schema.dart';
import 'package:fl_clash/widgets/widgets.dart';
import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import 'widgets/core_status_button.dart';
import 'widgets/start_button.dart';

const _maxGridWidth = 280.0 * 16 / 4;

/// 仪表盘：完全由 UI schema 驱动（用户 2026-11-26 明确不要拖拽编辑）。
///
/// 布局来源：`ui_presets/<当前预设>.json` → 副本 `ui.json` → `defaultUiSchema()`。
/// 改布局 = 换预设或导入 JSON，不用重新编译。
class DashboardView extends ConsumerWidget {
  const DashboardView({super.key});

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final page =
        ref.watch(uiSchemaProvider).value?.schema.page('dashboard') ??
        defaultUiSchema().page('dashboard')!;
    return CommonScaffold(
      title: context.appLocalizations.dashboard,
      actions: [if (coreLib == null) const CoreStatusButton()],
      floatingActionButton: const StartButton(),
      body: Align(
        alignment: Alignment.topCenter,
        child: SingleChildScrollView(
          padding: const EdgeInsets.all(16).copyWith(bottom: 88),
          child: Align(
            alignment: Alignment.topCenter,
            child: ConstrainedBox(
              constraints: const BoxConstraints(maxWidth: _maxGridWidth),
              child: UiPageBody(page: page),
            ),
          ),
        ),
      ),
    );
  }
}
