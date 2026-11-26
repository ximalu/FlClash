/// UI 布局：预设库 + 当前生效的 schema。
///
/// 存储：
/// - `<HomeDir>/ui_presets/<名称>.json`  预设库（名称 = 文件名）
/// - `<HomeDir>/ui.json`                 当前生效的副本（保持旧路径，进备份）
/// - `<HomeDir>/ui_active`               当前生效的预设名（纯文本）
///
/// 规则（用户 2026-11-26）：
/// - 导入失败**不应用**，直接把错误返回给 UI；
/// - `默认风格` 永远存在、不可删除；
/// - 用户删掉其它内置预设，不会被自动重建。
library;

import 'dart:io';

import 'package:fl_clash/common/common.dart';
import 'package:fl_clash/ui/ui_cards.dart';
import 'package:fl_clash/ui/ui_presets.dart';
import 'package:fl_clash/ui/ui_schema.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:path/path.dart' as p;

class UiPresetEntry {
  final String name;
  final bool builtin;

  const UiPresetEntry(this.name, {this.builtin = false});
}

class UiSchemaState {
  final UiSchema schema;
  final String activeName;

  /// true = 当前不是「默认风格」。
  final bool custom;
  final List<String> warnings;
  final String? error;
  final List<UiPresetEntry> presets;

  const UiSchemaState({
    required this.schema,
    this.activeName = kDefaultPresetName,
    this.custom = false,
    this.warnings = const [],
    this.error,
    this.presets = const [],
  });
}

final uiSchemaProvider =
    AsyncNotifierProvider<UiSchemaNotifier, UiSchemaState>(
      UiSchemaNotifier.new,
    );

class UiSchemaNotifier extends AsyncNotifier<UiSchemaState> {
  static const String schemaFileName = 'ui.json';
  static const String presetsDirName = 'ui_presets';
  static const String activeFileName = 'ui_active';

  Future<Directory> _presetsDir() async {
    final home = await appPath.homeDirPath;
    return Directory(p.join(home, presetsDirName));
  }

  Future<File> _schemaFile() async {
    final home = await appPath.homeDirPath;
    return File(p.join(home, schemaFileName));
  }

  Future<File> _activeFile() async {
    final home = await appPath.homeDirPath;
    return File(p.join(home, activeFileName));
  }

  /// 文件名不能带路径分隔符；空名换成时间戳。
  String _sanitize(String raw) {
    final name = raw.replaceAll(RegExp(r'[\\/:*?"<>|]'), '_').trim();
    if (name.isEmpty) {
      return '未命名-${DateTime.now().millisecondsSinceEpoch}';
    }
    if (name.length > 40) {
      return name.substring(0, 40);
    }
    return name;
  }

  Future<void> _ensureBuiltins(Directory dir) async {
    final existed = await dir.exists();
    if (!existed) {
      await dir.create(recursive: true);
    }
    // 目录已有内容 = 不是首次运行；此时只补「默认风格」，尊重用户删掉的其它预设。
    final hasFiles = existed && !await dir.list().isEmpty;
    for (final preset in builtinPresets()) {
      final file = File(p.join(dir.path, '${preset.name}.json'));
      if (await file.exists()) {
        continue;
      }
      if (preset.name != kDefaultPresetName && hasFiles) {
        continue;
      }
      await file.writeAsString(preset.schema.encode(), flush: true);
    }
  }

  Future<List<UiPresetEntry>> _listPresets(Directory dir) async {
    final builtinNames = builtinPresets().map((e) => e.name).toSet();
    final names = <String>[];
    if (await dir.exists()) {
      await for (final entity in dir.list()) {
        if (entity is File && entity.path.endsWith('.json')) {
          names.add(p.basenameWithoutExtension(entity.path));
        }
      }
    }
    names.sort((a, b) {
      if (a == kDefaultPresetName) return -1;
      if (b == kDefaultPresetName) return 1;
      return a.compareTo(b);
    });
    return [
      for (final name in names)
        UiPresetEntry(name, builtin: builtinNames.contains(name)),
    ];
  }

  @override
  Future<UiSchemaState> build() async {
    try {
      final dir = await _presetsDir();
      await _ensureBuiltins(dir);
      final presets = await _listPresets(dir);

      // 当前生效：优先 ui.json，其次 ui_active 指定的预设，最后默认。
      final schemaFile = await _schemaFile();
      var activeName = kDefaultPresetName;
      final activeFile = await _activeFile();
      if (await activeFile.exists()) {
        final raw = (await activeFile.readAsString()).trim();
        if (raw.isNotEmpty) {
          activeName = raw;
        }
      }
      final presetFile = File(p.join(dir.path, '$activeName.json'));
      final source = await schemaFile.exists()
          ? await schemaFile.readAsString()
          : (await presetFile.exists()
                ? await presetFile.readAsString()
                : defaultUiSchema().encode());
      final result = parseUiSchema(source, knownCards: uiKnownCardIds);
      if (!result.ok) {
        return UiSchemaState(
          schema: defaultUiSchema(),
          activeName: kDefaultPresetName,
          warnings: result.warnings,
          error: result.error,
          presets: presets,
        );
      }
      return UiSchemaState(
        schema: result.schema!,
        activeName: activeName,
        custom: activeName != kDefaultPresetName,
        warnings: result.warnings,
        presets: presets,
      );
    } catch (e) {
      return UiSchemaState(
        schema: defaultUiSchema(),
        error: '$e',
      );
    }
  }

  Future<String?> applyPreset(String name) async {
    try {
      final dir = await _presetsDir();
      final file = File(p.join(dir.path, '$name.json'));
      if (!await file.exists()) {
        return '预设不存在：$name';
      }
      final text = await file.readAsString();
      final result = parseUiSchema(text, knownCards: uiKnownCardIds);
      if (!result.ok) {
        return result.error;
      }
      await _writeActive(result.schema!, name);
      return null;
    } catch (e) {
      return '应用失败：$e';
    }
  }

  /// 导入新预设并应用。解析失败**不落盘、不应用**。
  Future<String?> importAsPreset(String rawName, String text) async {
    final result = parseUiSchema(text, knownCards: uiKnownCardIds);
    if (!result.ok) {
      return result.error;
    }
    final name = _sanitize(rawName);
    try {
      final dir = await _presetsDir();
      if (!await dir.exists()) {
        await dir.create(recursive: true);
      }
      final file = File(p.join(dir.path, '$name.json'));
      await file.writeAsString(result.schema!.encode(), flush: true);
      await _writeActive(result.schema!, name);
      return null;
    } catch (e) {
      return '写入失败：$e';
    }
  }

  Future<String?> deletePreset(String name) async {
    if (name == kDefaultPresetName) {
      return '「$kDefaultPresetName」不可删除';
    }
    try {
      final dir = await _presetsDir();
      final file = File(p.join(dir.path, '$name.json'));
      if (await file.exists()) {
        await file.delete();
      }
      final current = state.value;
      if (current != null && current.activeName == name) {
        return await applyPreset(kDefaultPresetName);
      }
      state = AsyncData(current == null ? await build() : await _refreshState());
      return null;
    } catch (e) {
      return '删除失败：$e';
    }
  }

  Future<String> exportPreset(String name) async {
    try {
      final dir = await _presetsDir();
      final file = File(p.join(dir.path, '$name.json'));
      if (await file.exists()) {
        return await file.readAsString();
      }
    } catch (_) {
      // 落到下面的默认导出。
    }
    return (state.value?.schema ?? defaultUiSchema()).encode();
  }

  Future<String?> resetToDefault() => applyPreset(kDefaultPresetName);

  Future<void> _writeActive(UiSchema schema, String name) async {
    final schemaFile = await _schemaFile();
    await schemaFile.parent.create(recursive: true);
    await schemaFile.writeAsString(schema.encode(), flush: true);
    final activeFile = await _activeFile();
    await activeFile.writeAsString(name, flush: true);
    final dir = await _presetsDir();
    state = AsyncData(
      UiSchemaState(
        schema: schema,
        activeName: name,
        custom: name != kDefaultPresetName,
        presets: await _listPresets(dir),
      ),
    );
  }

  Future<UiSchemaState> _refreshState() async {
    final previous = state.value;
    final dir = await _presetsDir();
    return UiSchemaState(
      schema: previous?.schema ?? defaultUiSchema(),
      activeName: previous?.activeName ?? kDefaultPresetName,
      custom: previous?.custom ?? false,
      warnings: previous?.warnings ?? const [],
      error: previous?.error,
      presets: await _listPresets(dir),
    );
  }
}
