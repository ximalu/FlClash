import 'dart:io';

import 'package:fl_clash/enum/enum.dart';
import 'package:fl_clash/models/models.dart';
import 'package:fl_clash/providers/app.dart';
import 'package:fl_clash/state.dart';
import 'package:flutter/material.dart';
import 'package:path_provider/path_provider.dart';

class CommonPrint {
  static CommonPrint? _instance;

  CommonPrint._internal();

  factory CommonPrint() {
    _instance ??= CommonPrint._internal();
    return _instance!;
  }

  // FlClashTier: Dart 日志同时落盘（方案 A，2026-08-15）
  static const String _logDirName = 'logs';
  static const int _maxLogBytes = 512 * 1024; // 512KB 滚动上限
  static File? _logFile;

  Future<File?> _getLogFile() async {
    if (_logFile != null) return _logFile;
    try {
      final dir = await getApplicationSupportDirectory();
      final logDir = Directory('${dir.path}/$_logDirName');
      if (!await logDir.exists()) {
        await logDir.create(recursive: true);
      }
      final file = File('${logDir.path}/dart.log');
      _logFile = file;
      return file;
    } catch (_) {
      return null;
    }
  }

  Future<void> _writeToFile(String text) async {
    final file = await _getLogFile();
    if (file == null) return;
    try {
      await file.writeAsString(text, mode: FileMode.append);
      final len = await file.length();
      if (len > _maxLogBytes) {
        final content = await file.readAsString();
        await file.writeAsString(content.substring(content.length - _maxLogBytes ~/ 2));
      }
    } catch (_) {}
  }

  void log(String? text, {LogLevel logLevel = LogLevel.info}) {
    final payload = '[APP] $text';
    debugPrint(payload);
    // FlClashTier: 落盘（异步，不阻塞 UI）
    final now = DateTime.now();
    final ts = '${now.year}-${now.month.toString().padLeft(2, '0')}-${now.day.toString().padLeft(2, '0')} '
        '${now.hour.toString().padLeft(2, '0')}:${now.minute.toString().padLeft(2, '0')}:${now.second.toString().padLeft(2, '0')}.${now.millisecond.toString().padLeft(3, '0')}';
    _writeToFile('[$ts][${logLevel.name}] $payload\n');
    if (!globalState.isAttach) {
      return;
    }
    globalState.container
        .read(logsProvider.notifier)
        .add(Log.app(payload).copyWith(logLevel: logLevel));
  }
}

final commonPrint = CommonPrint();
