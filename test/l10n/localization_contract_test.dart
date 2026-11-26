import 'dart:convert';
import 'dart:io';

import 'package:fl_clash/l10n/intl/messages_en.dart' as messages_en;
import 'package:fl_clash/l10n/intl/messages_ja.dart' as messages_ja;
import 'package:fl_clash/l10n/intl/messages_ru.dart' as messages_ru;
import 'package:fl_clash/l10n/intl/messages_zh_CN.dart' as messages_zh_cn;
import 'package:flutter_test/flutter_test.dart';
import 'package:intl/message_lookup_by_library.dart';

void main() {
  final lookups = <String, MessageLookupByLibrary>{
    'en': messages_en.messages,
    'ja': messages_ja.messages,
    'ru': messages_ru.messages,
    'zh_CN': messages_zh_cn.messages,
  };

  test('every generated locale exposes and evaluates every source message', () {
    final arbByLocale = <String, Map<String, dynamic>>{
      for (final locale in lookups.keys)
        locale:
            jsonDecode(File('arb/intl_$locale.arb').readAsStringSync())
                as Map<String, dynamic>,
    };
    final expectedKeys = arbByLocale['en']!.keys
        .where((key) => !key.startsWith('@'))
        .toSet();

    for (final entry in lookups.entries) {
      final locale = entry.key;
      final lookup = entry.value;
      final messages = lookup.messages;
      expect(
        messages.keys.toSet(),
        expectedKeys,
        reason: '$locale generated messages must match the English contract',
      );

      for (final key in expectedKeys) {
        final template = arbByLocale['en']![key] as String;
        final placeholderNames = RegExp(
          r'\{([A-Za-z_][A-Za-z0-9_]*)\b',
        ).allMatches(template).map((match) => match.group(1)).toSet();
        final argumentCount = placeholderNames.length;
        // 无占位符的 key 在 generated map 里是 simpleMessage 对象，不是函数，跳过。
        if (argumentCount == 0) {
          continue;
        }
        final message = messages[key]!;
        expect(
          message,
          isA<Function>(),
          reason: '$locale.$key 有占位符但生成的是非函数消息（本地化手写遗漏）',
        );
        final arguments = List<dynamic>.filled(argumentCount, 2);
        late final dynamic translated;
        try {
          translated = Function.apply(message as Function, arguments);
        } on NoSuchMethodError catch (error) {
          fail('$locale.$key has mismatched placeholder metadata: $error');
        }

        // 直接挡住 issues-backlog #13 那类回归：占位符原样上屏（「接收 {a} · 发送 {b}」）。
        expect(
          translated,
          isNot(contains(RegExp(r'\{[A-Za-z_][A-Za-z0-9_]*\}'))),
          reason: '$locale.$key 仍有未替换的占位符：$translated',
        );

        expect(
          translated,
          isA<String>(),
          reason: '$locale.$key must evaluate to text',
        );
        expect(
          translated as String,
          isNotEmpty,
          reason: '$locale.$key must not be empty',
        );
      }
    }
  });
}
