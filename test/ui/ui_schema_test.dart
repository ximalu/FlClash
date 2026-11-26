import 'package:fl_clash/ui/ui_cards.dart';
import 'package:fl_clash/ui/ui_schema.dart';
import 'package:flutter_test/flutter_test.dart';

void main() {
  test('内置布局能无警告地往返', () {
    final result = parseUiSchema(
      defaultUiSchema().encode(),
      knownCards: uiKnownCardIds,
    );
    expect(result.ok, isTrue, reason: result.error);
    expect(result.warnings, isEmpty);
    expect(result.schema!.page('dashboard'), isNotNull);
    expect(result.schema!.page('network'), isNotNull);
  });

  test('未知卡片被丢弃且给出 warning', () {
    const source = '''
{"version":1,"pages":{"dashboard":{"layout":"grid","sections":[
  {"cards":[{"id":"networkSpeed","span":8},{"id":"notARealCard"}]}]}}}''';
    final result = parseUiSchema(source, knownCards: uiKnownCardIds);
    expect(result.ok, isTrue);
    expect(result.warnings.any((w) => w.contains('notARealCard')), isTrue);
    expect(result.schema!.page('dashboard')!.sections.single.cards.length, 1);
  });

  test('版本不匹配直接报错', () {
    final result = parseUiSchema(
      '{"version":999,"pages":{}}',
      knownCards: uiKnownCardIds,
    );
    expect(result.ok, isFalse);
    expect(result.error, contains('version'));
  });

  test('坏 JSON 不抛异常', () {
    final result = parseUiSchema('{oops', knownCards: uiKnownCardIds);
    expect(result.ok, isFalse);
    expect(result.error, isNotNull);
  });

  test('span 会被夹到 1..columns', () {
    const source = '''
{"version":1,"pages":{"dashboard":{"layout":"grid","columns":6,"sections":[
  {"cards":[{"id":"networkSpeed","span":99},{"id":"trafficUsage","span":0}]}]}}}''';
    final result = parseUiSchema(source, knownCards: uiKnownCardIds);
    final cards = result.schema!.page('dashboard')!.sections.single.cards;
    expect(cards[0].span, 6);
    expect(cards[1].span, 1);
  });

  test('空分区被丢弃；全都为空则整页丢弃并报错', () {
    const source = '''
{"version":1,"pages":{"dashboard":{"sections":[{"cards":[{"id":"nope"}]}]}}}''';
    final result = parseUiSchema(source, knownCards: uiKnownCardIds);
    expect(result.ok, isFalse);
  });
}
