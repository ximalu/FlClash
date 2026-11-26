/// UI 布局管理页：预设库 + 导入 / 导出 / 删除 / 恢复默认。
///
/// 用户 2026-11-26：
/// - 布局列表，名称 = 文件名（极简风格.json …）；点一下即应用；
/// - 导入保留「文件 / URL / 剪贴板」；
/// - 导出 / 删除；`默认风格` 不可删除；
/// - JSON 异常直接报错、**不应用**。
///
/// 设计文档：kb/projects/flclashtier/ui-schema-hotreload.md
library;

import 'dart:convert';
import 'dart:io';

import 'package:dio/dio.dart';
import 'package:file_picker/file_picker.dart';
import 'package:fl_clash/common/common.dart';
import 'package:fl_clash/providers/ui_schema.dart';
import 'package:fl_clash/ui/ui_presets.dart';
import 'package:fl_clash/widgets/widgets.dart';
import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

class UiLayoutView extends ConsumerWidget {
  const UiLayoutView({super.key});

  static const _defaultUrl = 'http://192.168.196.81:18081/shared/ui.json';

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final state = ref.watch(uiSchemaProvider).value;
    final presets = state?.presets ?? const <UiPresetEntry>[];
    return CommonScaffold(
      title: 'UI 布局',
      body: ListView(
        padding: const EdgeInsets.fromLTRB(12, 8, 12, 24),
        children: [
          generateSectionV2(
            title: '当前',
            items: [
              ListItem(
                title: Text('当前 UI：${state?.activeName ?? '…'}'),
                subtitle: Text(
                  state == null
                      ? '加载中…'
                      : (state.custom
                            ? '自定义预设（点下方任意一项切换）'
                            : '内置默认布局'),
                ),
                minVerticalPadding: 12,
              ),
              if (state?.error != null)
                ListItem(
                  title: const Text('上次读取失败，已回落默认布局'),
                  subtitle: Text(state!.error!),
                  minVerticalPadding: 12,
                ),
              if ((state?.warnings ?? const []).isNotEmpty)
                ListItem(
                  title: Text('已跳过 ${state!.warnings.length} 项'),
                  subtitle: Text(state.warnings.take(6).join('；')),
                  minVerticalPadding: 12,
                ),
            ],
          ),
          generateSectionV2(
            title: '布局列表',
            items: [
              for (final preset in presets)
                ListItem(
                  leading: Icon(
                    preset.name == state?.activeName
                        ? Icons.check_circle
                        : Icons.dashboard_customize_outlined,
                    color: preset.name == state?.activeName
                        ? context.colorScheme.primary
                        : null,
                  ),
                  title: Text(preset.name),
                  subtitle: Text(
                    [
                      if (preset.builtin) '内置' else '导入',
                      if (preset.name == kDefaultPresetName) '不可删除',
                      if (preset.name == state?.activeName) '使用中',
                    ].join(' · '),
                  ),
                  minVerticalPadding: 12,
                  trailing: Row(
                    mainAxisSize: MainAxisSize.min,
                    children: [
                      IconButton(
                        tooltip: '复制 JSON',
                        icon: const Icon(Icons.copy_all_outlined, size: 20),
                        onPressed: () async {
                          final text = await ref
                              .read(uiSchemaProvider.notifier)
                              .exportPreset(preset.name);
                          await Clipboard.setData(ClipboardData(text: text));
                          if (context.mounted) {
                            context.showNotifier('已复制 ${preset.name}');
                          }
                        },
                      ),
                      if (preset.name != kDefaultPresetName)
                        IconButton(
                          tooltip: '删除',
                          icon: const Icon(Icons.delete_outline, size: 20),
                          onPressed: () => _delete(context, ref, preset.name),
                        ),
                    ],
                  ),
                  onTap: () => _apply(context, ref, preset.name),
                ),
            ],
          ),
          generateSectionV2(
            title: '导入为新预设',
            items: [
              ListItem(
                title: const Text('从文件'),
                subtitle: const Text('选一个 .json'),
                minVerticalPadding: 12,
                onTap: () => _importFile(context, ref),
              ),
              ListItem(
                title: const Text('从 URL'),
                subtitle: const Text(_defaultUrl),
                minVerticalPadding: 12,
                onTap: () => _importUrl(context, ref),
              ),
              ListItem(
                title: const Text('从剪贴板'),
                subtitle: const Text('把 JSON 粘贴进来'),
                minVerticalPadding: 12,
                onTap: () => _importClipboard(context, ref),
              ),
            ],
          ),
          generateSectionV2(
            title: '其它',
            items: [
              ListItem(
                title: const Text('恢复默认'),
                subtitle: const Text('应用「默认风格」'),
                minVerticalPadding: 12,
                onTap: () => _apply(context, ref, kDefaultPresetName),
              ),
            ],
          ),
        ],
      ),
    );
  }

  Future<void> _apply(BuildContext context, WidgetRef ref, String name) async {
    final error = await ref.read(uiSchemaProvider.notifier).applyPreset(name);
    if (!context.mounted) {
      return;
    }
    context.showNotifier(error == null ? '已应用 $name' : '应用失败：$error');
  }

  Future<void> _delete(
    BuildContext context,
    WidgetRef ref,
    String name,
  ) async {
    final confirmed = await showDialog<bool>(
      context: context,
      builder: (context) => AlertDialog(
        title: Text('删除 $name？'),
        content: const Text('只删预设文件，不影响其它设置。'),
        actions: [
          TextButton(
            onPressed: () => Navigator.of(context).pop(false),
            child: const Text('取消'),
          ),
          TextButton(
            onPressed: () => Navigator.of(context).pop(true),
            child: const Text('删除'),
          ),
        ],
      ),
    );
    if (confirmed != true) {
      return;
    }
    final error = await ref.read(uiSchemaProvider.notifier).deletePreset(name);
    if (context.mounted) {
      context.showNotifier(error == null ? '已删除 $name' : '删除失败：$error');
    }
  }

  Future<void> _importAsPreset(
    BuildContext context,
    WidgetRef ref,
    String text, {
    String? suggestedName,
  }) async {
    final name = await _askName(context, suggestedName);
    if (name == null || name.isEmpty) {
      return;
    }
    final error = await ref
        .read(uiSchemaProvider.notifier)
        .importAsPreset(name, text);
    if (!context.mounted) {
      return;
    }
    context.showNotifier(error == null ? '已导入并应用 $name' : '导入失败（未应用）：$error');
  }

  Future<String?> _askName(BuildContext context, String? suggested) async {
    final controller = TextEditingController(
      text: suggested ?? '我的风格 ${DateTime.now().month}-${DateTime.now().day}',
    );
    return showDialog<String>(
      context: context,
      builder: (context) => AlertDialog(
        title: const Text('预设名称'),
        content: TextField(
          controller: controller,
          autofocus: true,
          maxLength: 40,
          decoration: const InputDecoration(hintText: '例如：极简风格'),
        ),
        actions: [
          TextButton(
            onPressed: () => Navigator.of(context).pop(),
            child: const Text('取消'),
          ),
          TextButton(
            onPressed: () => Navigator.of(context).pop(controller.text.trim()),
            child: const Text('保存并应用'),
          ),
        ],
      ),
    );
  }

  Future<void> _importFile(BuildContext context, WidgetRef ref) async {
    try {
      final result = await FilePicker.pickFiles(
        type: FileType.custom,
        allowedExtensions: const ['json'],
      );
      final file = result?.files.single;
      if (file == null) {
        return;
      }
      final path = file.path;
      final text = path != null
          ? await File(path).readAsString()
          : utf8.decode(await file.readAsBytes());
      final name = path
          ?.split(Platform.pathSeparator)
          .last
          .replaceAll('.json', '');
      if (context.mounted) {
        await _importAsPreset(context, ref, text, suggestedName: name);
      }
    } catch (e) {
      if (context.mounted) {
        context.showNotifier('读取失败：$e');
      }
    }
  }

  Future<void> _importClipboard(BuildContext context, WidgetRef ref) async {
    final data = await Clipboard.getData(Clipboard.kTextPlain);
    final text = data?.text;
    if (text == null || text.trim().isEmpty) {
      if (context.mounted) {
        context.showNotifier('剪贴板是空的');
      }
      return;
    }
    if (context.mounted) {
      await _importAsPreset(context, ref, text);
    }
  }

  Future<void> _importUrl(BuildContext context, WidgetRef ref) async {
    final controller = TextEditingController(text: _defaultUrl);
    final url = await showDialog<String>(
      context: context,
      builder: (context) => AlertDialog(
        title: const Text('从 URL 导入'),
        content: TextField(
          controller: controller,
          autocorrect: false,
          decoration: const InputDecoration(hintText: 'http(s)://…/ui.json'),
        ),
        actions: [
          TextButton(
            onPressed: () => Navigator.of(context).pop(),
            child: const Text('取消'),
          ),
          TextButton(
            onPressed: () => Navigator.of(context).pop(controller.text.trim()),
            child: const Text('下载'),
          ),
        ],
      ),
    );
    if (url == null || url.isEmpty) {
      return;
    }
    try {
      final response = await Dio(
        BaseOptions(
          connectTimeout: const Duration(seconds: 8),
          receiveTimeout: const Duration(seconds: 8),
        ),
      ).get<String>(url);
      final text = response.data;
      if (text == null || text.isEmpty) {
        if (context.mounted) {
          context.showNotifier('URL 返回空内容');
        }
        return;
      }
      final name = url.split('/').last.replaceAll('.json', '');
      if (context.mounted) {
        await _importAsPreset(context, ref, text, suggestedName: name);
      }
    } catch (e) {
      if (context.mounted) {
        context.showNotifier('下载失败：$e');
      }
    }
  }
}
