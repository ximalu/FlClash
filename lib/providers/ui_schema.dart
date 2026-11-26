/// UI 布局 schema 的加载 / 导入 / 导出 / 重置。
///
/// 存储：`<HomeDir>/ui.json`（独立文件，不塞进 config.json，避免动 freezed 模型和 codegen）。
/// 解析失败一律回落内置布局（`defaultUiSchema()`），绝不白屏。
library;

import 'dart:io';

import 'package:fl_clash/common/common.dart';
import 'package:fl_clash/ui/ui_cards.dart';
import 'package:fl_clash/ui/ui_schema.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:path/path.dart' as p;

class UiSchemaState {
  final UiSchema schema;

  /// true = 来自 ui.json；false = 内置布局。
  final bool custom;
  final List<String> warnings;

  /// 上次读取/导入的错误（有值时界面用内置布局并提示）。
  final String? error;

  const UiSchemaState({
    required this.schema,
    this.custom = false,
    this.warnings = const [],
    this.error,
  });
}

final uiSchemaProvider =
    AsyncNotifierProvider<UiSchemaNotifier, UiSchemaState>(
      UiSchemaNotifier.new,
    );

class UiSchemaNotifier extends AsyncNotifier<UiSchemaState> {
  static const String fileName = 'ui.json';

  Future<File> _file() async {
    final home = await appPath.homeDirPath;
    return File(p.join(home, fileName));
  }

  @override
  Future<UiSchemaState> build() async {
    try {
      final file = await _file();
      if (!await file.exists()) {
        return UiSchemaState(schema: defaultUiSchema());
      }
      final text = await file.readAsString();
      final result = parseUiSchema(text, knownCards: uiKnownCardIds);
      if (!result.ok) {
        return UiSchemaState(
          schema: defaultUiSchema(),
          warnings: result.warnings,
          error: result.error,
        );
      }
      return UiSchemaState(
        schema: result.schema!,
        custom: true,
        warnings: result.warnings,
      );
    } catch (e) {
      return UiSchemaState(schema: defaultUiSchema(), error: '$e');
    }
  }

  /// 导入一段 JSON。成功返回 null，失败返回错误文案（状态不变）。
  Future<String?> importFromString(String text) async {
    final result = parseUiSchema(text, knownCards: uiKnownCardIds);
    if (!result.ok) {
      return result.error;
    }
    try {
      final file = await _file();
      await file.parent.create(recursive: true);
      await file.writeAsString(result.schema!.encode(), flush: true);
    } catch (e) {
      return '写入失败：$e';
    }
    state = AsyncData(
      UiSchemaState(
        schema: result.schema!,
        custom: true,
        warnings: result.warnings,
      ),
    );
    return null;
  }

  /// 导出当前生效的布局（含拖拽前的默认值），给外部做变体的底板。
  String exportJson() {
    final value = state.value;
    return (value?.schema ?? defaultUiSchema()).encode();
  }

  Future<void> reset() async {
    try {
      final file = await _file();
      if (await file.exists()) {
        await file.delete();
      }
    } catch (_) {
      // 删不掉也不影响：内存里已经回落内置布局。
    }
    state = AsyncData(UiSchemaState(schema: defaultUiSchema()));
  }
}
