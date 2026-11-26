/// UI 布局管理页：导入 / 导出 / 重置 ui.json。
///
/// 用户 2026-11-26 定的边界：**不在手机上编辑 UI**，只需要「导入一个文件就看到新界面」。
/// 所以这里只有入口，没有可视化编辑器。
///
/// 设计文档：kb/projects/flclashtier/ui-schema-hotreload.md
library;

import 'dart:convert';
import 'dart:io';

import 'package:dio/dio.dart';
import 'package:file_picker/file_picker.dart';
import 'package:fl_clash/common/common.dart';
import 'package:fl_clash/providers/ui_schema.dart';
import 'package:fl_clash/widgets/widgets.dart';
import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

class UiLayoutView extends ConsumerWidget {
  const UiLayoutView({super.key});

  // TODO(l10n): 这几条文案目前硬编码，正式发版前补进 4 个 locale。
  static const _defaultUrl = 'http://192.168.196.81:18081/shared/ui.json';

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final state = ref.watch(uiSchemaProvider).value;
    final isCustom = state?.custom ?? false;
    return CommonScaffold(
      title: 'UI 布局',
      body: ListView(
        padding: const EdgeInsets.fromLTRB(12, 8, 12, 24),
        children: [
          generateSectionV2(
            title: '当前',
            items: [
              ListItem(
                title: Text(isCustom ? '自定义布局' : '内置布局'),
                subtitle: Text(
                  state == null
                      ? '加载中…'
                      : (isCustom
                            ? '来源：ui.json（导入即生效，无需重启）'
                            : '未导入 ui.json'),
                ),
                minVerticalPadding: 12,
              ),
              if (state?.error != null)
                ListItem(
                  title: const Text('上次解析失败，已回落内置布局'),
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
            title: '导入',
            items: [
              ListItem(
                title: const Text('从文件'),
                subtitle: const Text('选一个 ui.json'),
                minVerticalPadding: 12,
                onTap: () => _importFile(context, ref),
              ),
              ListItem(
                title: const Text('从剪贴板'),
                subtitle: const Text('直接把 JSON 粘贴进来'),
                minVerticalPadding: 12,
                onTap: () => _importClipboard(context, ref),
              ),
              ListItem(
                title: const Text('从 URL'),
                subtitle: const Text(_defaultUrl),
                minVerticalPadding: 12,
                onTap: () => _importUrl(context, ref),
              ),
            ],
          ),
          generateSectionV2(
            title: '导出 / 重置',
            items: [
              ListItem(
                title: const Text('复制当前布局'),
                subtitle: const Text('导出 JSON，拿去做下一版变体'),
                minVerticalPadding: 12,
                onTap: () async {
                  final text = ref.read(uiSchemaProvider.notifier).exportJson();
                  await Clipboard.setData(ClipboardData(text: text));
                  if (context.mounted) {
                    context.showNotifier('已复制到剪贴板');
                  }
                },
              ),
              ListItem(
                title: const Text('恢复内置布局'),
                subtitle: const Text('删除 ui.json，回到 0.9.22 的布局'),
                minVerticalPadding: 12,
                onTap: () async {
                  await ref.read(uiSchemaProvider.notifier).reset();
                  if (context.mounted) {
                    context.showNotifier('已恢复内置布局');
                  }
                },
              ),
            ],
          ),
        ],
      ),
    );
  }

  Future<void> _apply(BuildContext context, WidgetRef ref, String text) async {
    final error = await ref.read(uiSchemaProvider.notifier).importFromString(text);
    if (!context.mounted) {
      return;
    }
    if (error == null) {
      context.showNotifier('已应用新布局');
    } else {
      context.showNotifier('导入失败：$error');
    }
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
      if (context.mounted) {
        await _apply(context, ref, text);
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
      await _apply(context, ref, text);
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
            child: const Text('导入'),
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
      if (context.mounted) {
        await _apply(context, ref, text);
      }
    } catch (e) {
      if (context.mounted) {
        context.showNotifier('下载失败：$e');
      }
    }
  }
}
