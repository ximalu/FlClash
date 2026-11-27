import 'dart:io';

import 'package:fl_clash/common/common.dart';
import 'package:fl_clash/enum/enum.dart';
import 'package:fl_clash/models/models.dart';
import 'package:fl_clash/providers/providers.dart';
import 'package:fl_clash/state.dart';
import 'package:fl_clash/widgets/widgets.dart';
import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:super_sliver_list/super_sliver_list.dart';

/// 日志页的两个来源。分开的理由（2026-11-27）：
/// 以前 ZeroTier 事件与 mihomo 连接日志共用一条流、一个缓冲。实测一份 29 分钟的
/// 导出里 `[TCP]` 470 行、ZT 事件只有 274 行 —— 业务日志把 ZT 证据挤到只剩
/// 20–30 分钟，而这恰好是换网排障最需要的那段历史。现在 ZT 事件从 core 独立
/// 推上来（ztLog 事件），有自己的 5000 条缓冲，不再被顶掉。
enum _LogScope { zeroTier, other }

class LogsView extends ConsumerStatefulWidget {
  const LogsView({super.key});

  @override
  ConsumerState<LogsView> createState() => _LogsViewState();
}

class _LogsViewState extends ConsumerState<LogsView> {
  final _logsStateNotifier = ValueNotifier<LogsState>(const LogsState());
  late ScrollController _scrollController;

  List<Log> _logs = [];

  /// 默认停在 ZeroTier 页签：排障时先看它，而不必在几千行业务日志里翻。
  _LogScope _scope = _LogScope.zeroTier;

  @override
  void initState() {
    super.initState();
    _logs = ref.read(logsProvider).list;
    _scrollController = ScrollController(initialScrollOffset: double.maxFinite);
    _logsStateNotifier.value = _logsStateNotifier.value.copyWith(logs: _logs);
    ref.listenManual(logsProvider.select((state) => VM(state.list)), (
      prev,
      next,
    ) {
      if (prev != next) {
        final isEquality = logListEquality.equals(prev?.a, next.a);
        if (!isEquality) {
          _logs = next.a;
          updateLogsThrottler();
        }
      }
    });
  }

  void _onSearch(String value) {
    _logsStateNotifier.value = _logsStateNotifier.value.copyWith(query: value);
  }

  void _onKeywordsUpdate(List<String> keywords) {
    _logsStateNotifier.value = _logsStateNotifier.value.copyWith(
      keywords: keywords,
    );
  }

  @override
  void dispose() {
    _logsStateNotifier.dispose();
    _scrollController.dispose();
    super.dispose();
  }

  /// 当前页签的内存列表（导出与清空都按页签作用）。
  List<Log> get _currentLogs => _scope == _LogScope.zeroTier
      ? ref.read(ztLogsProvider).list
      : _logsStateNotifier.value.list;

  Future<void> _handleClear() async {
    // 2026-11-27 改语义：清空只清「界面里看得到的列表」，**不再截断诊断文件**。
    // 旧实现会把 zt-datapath.log / zt-diag.log / app-diag.log 一起清零 ——
    // 一次误点就等于把几十小时的证据删掉，而且不可恢复。文件交给自动滚动管。
    if (_scope == _LogScope.zeroTier) {
      ref.read(ztLogsProvider.notifier).clear();
    } else {
      final notifier = ref.read(logsProvider.notifier);
      // 保留原 maxLength：FixedList(0) 会把容量设成 0，之后新日志全被 truncate 丢弃
      notifier.value = FixedList(notifier.value.maxLength);
      _logs = [];
      _logsStateNotifier.value = _logsStateNotifier.value.copyWith(
        logs: const [],
      );
    }
    if (_scrollController.hasClients) {
      _scrollController.jumpTo(0);
    }
  }

  Future<void> _handleExport() async {
    final appLocalizations = context.appLocalizations;
    final filteredLogs = _currentLogs;
    final scopeName = _scope == _LogScope.zeroTier ? 'zeroTier' : 'other';
    final res = await globalState.safeRun<bool>(() async {
      var logString = _buildExportHeader(filteredLogs, scopeName);
      logString += await encodeLogsTask(filteredLogs);
      // 以下都是 <home>/logs/ 下的落盘文件：内存缓冲会滚动丢弃，这些文件保存
      // 更长的历史。**每个文件都连 .1 历史档一起导出** —— 只有当前文件时，
      // 滚动一次前半段就永久拿不到了（2026-11-27 实测：diag 段只覆盖 28–34 分钟）。
      logString += await _readAuxLog('zt-events.log', 'ZT events log');
      logString += await _readAuxLog('zt-events.log.1', 'ZT events log (prev)');
      logString += await _readAuxLog('zt-datapath.log', 'ZT datapath log');
      logString += await _readAuxLog('zt-datapath.log.1', 'ZT datapath log (prev)');
      logString += await _readAuxLog('zt-diag.log', 'ZT diag snapshots');
      logString += await _readAuxLog(
        'zt-diag.log.1',
        'ZT diag snapshots (prev)',
      );
      logString += await _readAuxLog('app-diag.log', 'App/network diag snapshots');
      final tempFilePath = await appPath.tempFilePath;
      final file = File(tempFilePath);
      await file.safeWriteAsString(logString);
      return await picker.saveFileWithPath(utils.logFile, tempFilePath) != null;
    }, title: appLocalizations.exportLogs);
    if (res != true) return;
    globalState.showMessage(
      title: appLocalizations.tip,
      message: TextSpan(text: appLocalizations.exportSuccess),
    );
  }

  /// 导出日志的文件头（2026-11-27 用户提出：日志必须自带版本）。
  ///
  /// 以前每份日志都要靠 patchLevel、某几条新增日志去反推「这是哪个版本」，
  /// 装错版本时会直接影响所有结论。这里把版本号、导出时间、页签、内存缓冲覆盖
  /// 范围写在最前面 —— 第一眼就知道「这份日志属于哪个 APK、哪一段、哪个来源」。
  String _buildExportHeader(List<Log> logs, String scopeName) {
    final info = globalState.packageInfo;
    final buffer = StringBuffer()
      ..writeln('===== FlClashTier log export =====')
      ..writeln('appVersion: ${info.version}+${info.buildNumber}')
      ..writeln('packageName: ${info.packageName}')
      ..writeln('exportedAt: ${DateTime.now().showFull}')
      ..writeln('scope: $scopeName')
      ..writeln(
        'platform: ${Platform.operatingSystem} ${Platform.operatingSystemVersion}',
      )
      ..writeln('bufferEntries: ${logs.length}');
    if (logs.isNotEmpty) {
      buffer
        ..writeln('bufferFirst: ${logs.first.dateTime}')
        ..writeln('bufferLast: ${logs.last.dateTime}');
    }
    buffer.writeln('===== end of header =====');
    return buffer.toString();
  }

  Future<String> _readAuxLog(String name, String title) async {
    try {
      final home = await appPath.homeDirPath;
      final file = File('$home/logs/$name');
      if (!await file.exists()) return '';
      final text = await file.readAsString();
      if (text.trim().isEmpty) return '';
      return '\n\n===== $title =====\n$text';
    } catch (_) {
      return '';
    }
  }

  void updateLogsThrottler() {
    throttler.call(FunctionTag.logs, () {
      if (!mounted) {
        return;
      }
      final isEquality = logListEquality.equals(
        _logs,
        _logsStateNotifier.value.logs,
      );
      if (isEquality) {
        return;
      }
      WidgetsBinding.instance.addPostFrameCallback((_) {
        if (mounted) {
          _logsStateNotifier.value = _logsStateNotifier.value.copyWith(
            logs: _logs,
          );
        }
      });
    }, duration: commonDuration);
  }

  Widget _buildScopeBar(BuildContext context) {
    final appLocalizations = context.appLocalizations;
    return Padding(
      padding: const EdgeInsets.fromLTRB(16, 8, 16, 8),
      child: Row(
        children: [
          SegmentedButton<_LogScope>(
            segments: [
              ButtonSegment(
                value: _LogScope.zeroTier,
                label: Text(appLocalizations.zeroTier),
              ),
              ButtonSegment(
                value: _LogScope.other,
                label: Text(appLocalizations.other),
              ),
            ],
            selected: {_scope},
            showSelectedIcon: false,
            onSelectionChanged: (values) {
              setState(() => _scope = values.first);
            },
          ),
        ],
      ),
    );
  }

  List<Widget> _buildItems(
    BuildContext context,
    List<Log> logs,
    bool filterable,
  ) {
    return logs
        .map<Widget>(
          (log) => LogItem(
            key: Key('${log.dateTime}-${log.payload.hashCode}'),
            log: log,
            onClick: filterable
                ? (value) {
                    context.commonScaffoldState?.addKeyword(value);
                  }
                : null,
          ),
        )
        .separated(const Divider(height: 0))
        .toList();
  }

  Widget _buildList(
    BuildContext context, {
    required List<Log> logs,
    required bool autoScrollToEnd,
    required bool filterable,
  }) {
    final appLocalizations = context.appLocalizations;
    if (logs.isEmpty) {
      return NullStatus(
        illustration: const LogEmptyIllustration(),
        label: appLocalizations.nullTip(appLocalizations.logs),
      );
    }
    final items = _buildItems(context, logs, filterable);
    return Align(
      alignment: Alignment.topCenter,
      child: ScrollToEndBox(
        onCancelToEnd: () {
          _logsStateNotifier.value = _logsStateNotifier.value.copyWith(
            autoScrollToEnd: false,
          );
        },
        controller: _scrollController,
        enable: autoScrollToEnd,
        dataSource: logs,
        child: CommonScrollBar(
          controller: _scrollController,
          child: SuperListView.builder(
            physics: const NextClampingScrollPhysics(),
            reverse: true,
            shrinkWrap: true,
            controller: _scrollController,
            itemBuilder: (_, index) {
              return items[index];
            },
            itemCount: items.length,
          ),
        ),
      ),
    );
  }

  @override
  Widget build(BuildContext context) {
    final appLocalizations = context.appLocalizations;
    return CommonScaffold(
      onKeywordsUpdate: _onKeywordsUpdate,
      searchState: AppBarSearchState(onSearch: _onSearch),
      title: appLocalizations.logs,
      floatingActionButton: ValueListenableBuilder(
        valueListenable: _logsStateNotifier,
        builder: (_, state, _) {
          final autoScrollToEnd = state.autoScrollToEnd;
          return Column(
            mainAxisSize: MainAxisSize.min,
            crossAxisAlignment: CrossAxisAlignment.end,
            children: [
              FloatingActionButton.small(
                heroTag: 'clear_logs',
                tooltip: appLocalizations.clearLogs,
                onPressed: _handleClear,
                child: const Icon(Icons.delete_sweep_outlined),
              ),
              const SizedBox(height: 12),
              FloatingActionButton.small(
                heroTag: 'export_logs',
                tooltip: appLocalizations.exportLogs,
                onPressed: _handleExport,
                child: const Icon(Icons.save_as_outlined),
              ),
              const SizedBox(height: 12),
              FadeRotationScaleBox(
                child: FloatingActionButton(
                  key: ValueKey(autoScrollToEnd),
                  onPressed: () {
                    _logsStateNotifier.value = _logsStateNotifier.value.copyWith(
                      autoScrollToEnd: !_logsStateNotifier.value.autoScrollToEnd,
                    );
                  },
                  child: autoScrollToEnd
                      ? const Icon(Icons.block)
                      : const Icon(Icons.vertical_align_top),
                ),
              ),
            ],
          );
        },
      ),
      body: Column(
        children: [
          _buildScopeBar(context),
          Expanded(
            child: _scope == _LogScope.zeroTier
                ? ValueListenableBuilder<LogsState>(
                    valueListenable: _logsStateNotifier,
                    builder: (context, state, _) {
                      // ZeroTier 页签不套搜索过滤：它的价值就是「完整时间线」，
                      // 过滤后容易误判成「这段时间什么都没发生」。
                      return _buildList(
                        context,
                        logs: ref.watch(ztLogsProvider).list,
                        autoScrollToEnd: state.autoScrollToEnd,
                        filterable: false,
                      );
                    },
                  )
                : ValueListenableBuilder<LogsState>(
                    valueListenable: _logsStateNotifier,
                    builder: (context, state, _) {
                      return _buildList(
                        context,
                        logs: state.list,
                        autoScrollToEnd: state.autoScrollToEnd,
                        filterable: true,
                      );
                    },
                  ),
          ),
        ],
      ),
    );
  }
}

class LogItem extends StatelessWidget {
  final Log log;
  final Function(String)? onClick;

  const LogItem({super.key, required this.log, this.onClick});

  @override
  Widget build(BuildContext context) {
    return ListItem(
      padding: const EdgeInsets.symmetric(horizontal: 16, vertical: 4),
      onTap: () {},
      title: SelectableText(
        log.payload,
        style: context.textTheme.bodyLarge?.copyWith(
          color: log.logLevel.color(context),
        ),
      ),
      subtitle: Column(
        children: [
          const SizedBox(height: 16),
          Row(
            mainAxisAlignment: MainAxisAlignment.spaceBetween,
            children: [
              CommonChip(
                onPressed: () {
                  if (onClick == null) return;
                  onClick!(log.logLevel.name);
                },
                label: log.logLevel.name,
              ),
              Text(
                log.dateTime,
                style: context.textTheme.bodySmall?.copyWith(
                  color: context.colorScheme.onSurface.opacity80,
                ),
              ),
            ],
          ),
        ],
      ),
    );
  }
}
