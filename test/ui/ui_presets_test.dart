import 'package:fl_clash/ui/ui_cards.dart';
import 'package:fl_clash/ui/ui_presets.dart';
import 'package:fl_clash/ui/ui_schema.dart';
import 'package:flutter_test/flutter_test.dart';

void main() {
  test('所有内置预设都能无警告解析，且两页都在', () {
    for (final preset in builtinPresets()) {
      final result = parseUiSchema(
        preset.schema.encode(),
        knownCards: uiKnownCardIds,
      );
      expect(result.ok, isTrue, reason: '${preset.name}: ${result.error}');
      expect(result.warnings, isEmpty, reason: preset.name);
      expect(result.schema!.page('dashboard'), isNotNull, reason: preset.name);
      expect(result.schema!.page('network'), isNotNull, reason: preset.name);
    }
  });

  test('默认风格存在且包含默认页面', () {
    final presets = builtinPresets();
    expect(presets.first.name, kDefaultPresetName);
    expect(presets.length, greaterThanOrEqualTo(4));
  });

  test('textScale 被夹到 0.8..1.6', () {
    const source =
        '{"version":1,"pages":{"dashboard":{"textScale":9,"sections":'
        '[{"cards":[{"id":"networkSpeed"}]}]}}}';
    final result = parseUiSchema(source, knownCards: uiKnownCardIds);
    expect(result.schema!.page('dashboard')!.textScale, 1.6);
  });
}
