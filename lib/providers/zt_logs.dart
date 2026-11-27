import 'dart:async';

import 'package:fl_clash/common/common.dart';
import 'package:fl_clash/models/models.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

/// ZeroTier 事件在内存里保留多少条。
///
/// 实测 ZT 行约 570 条/小时（含每 10s 一条 datapath 摘要），5000 条 ≈ 8–9 小时，
/// 比旧的共用缓冲（500 条 ≈ 20–30 分钟，且被 [TCP]/[DNS] 挤占）长一个数量级。
/// 真正的长历史在文件层：`<home>/logs/zt-events.log`（4 MiB 滚动 + .1）。
const ztLogsMaxLength = 5000;

class ZtLogs extends Notifier<FixedList<Log>> {
  @override
  FixedList<Log> build() => FixedList(ztLogsMaxLength);

  void add(Log log) {
    if (!ref.mounted) {
      return;
    }
    state = state.copyWith()..add(log);
  }

  void clear() {
    final next = state.copyWith();
    next.clear();
    state = next;
  }
}

final ztLogsProvider = NotifierProvider<ZtLogs, FixedList<Log>>(ZtLogs.new);

/// ZeroTier 日志是否进「界面列表」。
///
/// 语义边界（必须在设置项副标题里写明）：诊断文件
/// （zt-events.log / zt-datapath.log / zt-diag.log）**始终写入**，不受这个开关
/// 影响 —— 证据不能被一个 UI 开关毁掉；开关只管界面里放不放。
class ZtLogEnabled extends Notifier<bool> {
  @override
  bool build() {
    unawaited(_load());
    return true;
  }

  Future<void> _load() async {
    final value = await Preferences().getZtLogCapture();
    if (ref.mounted) {
      state = value;
    }
  }

  Future<void> setEnabled(bool value) async {
    state = value;
    await Preferences().setZtLogCapture(value);
  }
}

final ztLogEnabledProvider = NotifierProvider<ZtLogEnabled, bool>(
  ZtLogEnabled.new,
);
