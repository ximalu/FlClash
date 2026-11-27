/// 内置 UI 预设（代码里生成，首次启动时落到 `<HomeDir>/ui_presets/`）。
///
/// 为什么不用 assets：预设就是几段 JSON，用代码构造能直接复用 `UiSchema` 类型与
/// `encode()`，不用另维护一份资源清单。用户删了非默认预设不会被自动重建。
///
/// 约定：`默认风格` 永远存在且不可删除（恢复默认就是应用它）。
library;

import 'ui_schema.dart';

const String kDefaultPresetName = '默认风格';

class BuiltinPreset {
  final String name;
  final UiSchema schema;

  const BuiltinPreset(this.name, this.schema);
}

UiPageSpec _networkPage() => defaultUiSchema().pages['network']!;

List<BuiltinPreset> builtinPresets() {
  final network = _networkPage();
  return [
    BuiltinPreset(kDefaultPresetName, defaultUiSchema()),
    BuiltinPreset(
      '极简风格',
      UiSchema(
        pages: {
          'dashboard': const UiPageSpec(
            layout: 'grid',
            sections: [
              UiSectionSpec(
                cards: [
                  UiCardSpec('networkSpeed', span: 8),
                  UiCardSpec('outboundSummary', span: 8),
                  UiCardSpec('networkDetection', span: 4),
                  UiCardSpec('trafficUsage', span: 4),
                ],
              ),
            ],
          ),
          'network': network,
        },
      ),
    ),
    BuiltinPreset(
      '横版风格',
      UiSchema(
        pages: {
          'dashboard': const UiPageSpec(
            layout: 'grid',
            sections: [
              UiSectionSpec(
                cards: [
                  UiCardSpec('outboundSummary', span: 8),
                  UiCardSpec('networkSpeed', span: 8),
                  UiCardSpec('networkDetection', span: 4),
                  UiCardSpec('intranetIp', span: 4),
                ],
              ),
            ],
          ),
          'network': network,
        },
      ),
    ),
    BuiltinPreset(
      '专业风格',
      UiSchema(
        pages: {
          'dashboard': const UiPageSpec(
            layout: 'grid',
            sections: [
              UiSectionSpec(
                cards: [
                  UiCardSpec('networkSpeed', span: 8),
                  UiCardSpec('outboundSummary', span: 8),
                ],
              ),
              UiSectionSpec(
                title: 'networkExits',
                cards: [
                  UiCardSpec('exitMihomo', span: 8),
                  UiCardSpec('exitZerotier', span: 8),
                ],
              ),
              UiSectionSpec(
                title: 'networkProbes',
                cards: [UiCardSpec('networkProbes', span: 8)],
              ),
              UiSectionSpec(
                cards: [
                  UiCardSpec('networkPeers', span: 8),
                  UiCardSpec('networkDiagnostics', span: 8),
                ],
              ),
            ],
          ),
          'network': network,
        },
      ),
    ),
    // 「大字风格」预设已随页面级 textScale 一起废弃（2026-11-27 定案 A）：
    // 字号改由系统/主题控制，内置预设不再提供字号档位。
  ];
}
