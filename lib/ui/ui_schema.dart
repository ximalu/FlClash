/// UI 布局 schema（v1）
///
/// 设计文档：kb/projects/flclashtier/ui-schema-hotreload.md
///
/// 原则（用户与朋友 2026-11-26 讨论后定）：
/// - **配置文件没有创造界面的能力**，只能引用 App 事先注册的卡片 id；
/// - 数据层/操作层固定，表现层（顺序、分组、跨列、显隐）可配置；
/// - 解析失败一律降级到内置布局，绝不白屏。
///
/// v1 只描述「页面 = 若干分区（可带标题）+ 卡片列表（可带跨列）」，
/// 卡片内部的字段组合留到 v2（composed 卡）。
library;

import 'dart:convert';

const int kUiSchemaVersion = 1;

/// 栅格列数：沿用 Dashboard 现有的 8 列（`SuperGrid`）。
const int kUiColumns = 8;

/// 一页最多的卡片数 / 分区数（防止恶意或手滑的巨大文件）。
const int kUiMaxCardsPerPage = 200;
const int kUiMaxSectionsPerPage = 40;

/// 一张卡片：只允许引用已注册的 id。
class UiCardSpec {
  final String id;

  /// 跨列数，仅对 `grid` 布局生效；1..columns。
  final int span;

  const UiCardSpec(this.id, {this.span = kUiColumns});

  Map<String, Object?> toJson() => {'id': id, 'span': span};
}

/// 一个分区（网络页的一节、Dashboard 的一整块）。
class UiSectionSpec {
  /// l10n key 名（如 `networkExits`）；null = 不显示标题。
  final String? title;
  final List<UiCardSpec> cards;

  const UiSectionSpec({this.title, required this.cards});

  Map<String, Object?> toJson() => {
    if (title != null) 'title': title,
    'cards': [for (final card in cards) card.toJson()],
  };
}

/// 一页。
class UiPageSpec {
  /// `grid`（Dashboard，8 列可跨列）或 `list`（网络页，纵向分区）。
  final String layout;
  final int columns;
  final List<UiSectionSpec> sections;

  const UiPageSpec({
    this.layout = 'list',
    this.columns = kUiColumns,
    required this.sections,
  });

  Map<String, Object?> toJson() => {
    'layout': layout,
    'columns': columns,
    'sections': [for (final section in sections) section.toJson()],
  };
}

class UiSchema {
  final int version;
  final Map<String, UiPageSpec> pages;

  const UiSchema({this.version = kUiSchemaVersion, required this.pages});

  UiPageSpec? page(String id) => pages[id];

  Map<String, Object?> toJson() => {
    'version': version,
    'pages': {
      for (final entry in pages.entries) entry.key: entry.value.toJson(),
    },
  };

  String encode() => const JsonEncoder.withIndent('  ').convert(toJson());
}

/// 解析结果：成功给 schema，失败给 error；两种都有 warnings（被丢弃的卡片等）。
class UiSchemaParseResult {
  final UiSchema? schema;
  final List<String> warnings;
  final String? error;

  const UiSchemaParseResult({this.schema, this.warnings = const [], this.error});

  bool get ok => schema != null;
}

/// 内置布局 = 0.9.22 的现状，1:1 复刻。
///
/// 存在意义有两个：解析失败时的兜底，以及「导出当前布局」的初始值。
UiSchema defaultUiSchema() => const UiSchema(
  pages: {
    'dashboard': UiPageSpec(
      layout: 'grid',
      sections: [
        UiSectionSpec(
          cards: [
            UiCardSpec('networkSpeed', span: 8),
            UiCardSpec('outboundModeV2', span: 8),
            UiCardSpec('outboundMode', span: 4),
            UiCardSpec('trafficUsage', span: 4),
            UiCardSpec('vpnButton', span: 4),
            UiCardSpec('networkDetection', span: 4),
            UiCardSpec('intranetIp', span: 4),
            UiCardSpec('memoryInfo', span: 4),
          ],
        ),
      ],
    ),
    'network': UiPageSpec(
      layout: 'list',
      sections: [
        UiSectionSpec(
          title: 'networkVpnService',
          cards: [UiCardSpec('networkService')],
        ),
        UiSectionSpec(
          title: 'networkExits',
          cards: [UiCardSpec('exitMihomo'), UiCardSpec('exitZerotier')],
        ),
        UiSectionSpec(
          title: 'networkProbes',
          cards: [UiCardSpec('networkProbes')],
        ),
        UiSectionSpec(
          cards: [UiCardSpec('networkPeers'), UiCardSpec('networkDiagnostics')],
        ),
      ],
    ),
  },
);

/// 解析 + 校验。`knownCards` 由调用方给（注册表是 UI 层的事，本文件不依赖 UI）。
UiSchemaParseResult parseUiSchema(
  String source, {
  required Set<String> knownCards,
}) {
  final Object? decoded;
  try {
    decoded = jsonDecode(source);
  } catch (e) {
    return UiSchemaParseResult(error: 'JSON 解析失败：$e');
  }
  if (decoded is! Map) {
    return const UiSchemaParseResult(error: '顶层必须是对象');
  }
  final version = decoded['version'];
  if (version is! int) {
    return const UiSchemaParseResult(error: '缺少 version');
  }
  if (version != kUiSchemaVersion) {
    return UiSchemaParseResult(error: '不支持的 version=$version（当前 $kUiSchemaVersion）');
  }
  final rawPages = decoded['pages'];
  if (rawPages is! Map || rawPages.isEmpty) {
    return const UiSchemaParseResult(error: '缺少 pages');
  }
  final warnings = <String>[];
  final pages = <String, UiPageSpec>{};
  for (final entry in rawPages.entries) {
    final pageId = entry.key.toString();
    final rawPage = entry.value;
    if (rawPage is! Map) {
      warnings.add('页面 $pageId 不是对象，已跳过');
      continue;
    }
    final layout = rawPage['layout'] == 'grid' ? 'grid' : 'list';
    final rawColumns = rawPage['columns'];
    final columns = rawColumns is int && rawColumns > 0 && rawColumns <= 24
        ? rawColumns
        : kUiColumns;
    final rawSections = rawPage['sections'];
    if (rawSections is! List) {
      warnings.add('页面 $pageId 缺少 sections，已跳过');
      continue;
    }
    final sections = <UiSectionSpec>[];
    for (final rawSection in rawSections.take(kUiMaxSectionsPerPage)) {
      if (rawSection is! Map) {
        warnings.add('页面 $pageId 有一个分区不是对象，已跳过');
        continue;
      }
      final rawCards = rawSection['cards'];
      if (rawCards is! List) {
        warnings.add('页面 $pageId 有分区缺少 cards，已跳过');
        continue;
      }
      final cards = <UiCardSpec>[];
      for (final rawCard in rawCards.take(kUiMaxCardsPerPage)) {
        if (rawCard is! Map) {
          warnings.add('页面 $pageId 有卡片不是对象，已跳过');
          continue;
        }
        final id = rawCard['id'];
        if (id is! String || id.isEmpty) {
          warnings.add('页面 $pageId 有卡片缺少 id，已跳过');
          continue;
        }
        if (!knownCards.contains(id)) {
          warnings.add('未知卡片 $id，已跳过');
          continue;
        }
        final rawSpan = rawCard['span'];
        final span = rawSpan is int ? rawSpan.clamp(1, columns) : columns;
        cards.add(UiCardSpec(id, span: span));
      }
      if (cards.isEmpty) {
        continue;
      }
      final rawTitle = rawSection['title'];
      sections.add(
        UiSectionSpec(
          title: rawTitle is String && rawTitle.isNotEmpty ? rawTitle : null,
          cards: cards,
        ),
      );
    }
    if (sections.isEmpty) {
      warnings.add('页面 $pageId 没有任何有效卡片，已跳过');
      continue;
    }
    pages[pageId] = UiPageSpec(
      layout: layout,
      columns: columns,
      sections: sections,
    );
  }
  if (pages.isEmpty) {
    return UiSchemaParseResult(error: '没有任何有效页面', warnings: warnings);
  }
  return UiSchemaParseResult(
    schema: UiSchema(version: version, pages: pages),
    warnings: warnings,
  );
}
