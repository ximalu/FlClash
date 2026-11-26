import 'package:fl_clash/common/common.dart';
import 'package:fl_clash/enum/enum.dart';
import 'package:fl_clash/providers/providers.dart';
import 'package:fl_clash/state.dart';
import 'package:fl_clash/widgets/widgets.dart';
import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:intl/intl.dart';

/// 横版「出站概览」：左边模式切换（横版胶囊），右边出站 IP + 国旗。
///
/// 用户 2026-11-26：横版出站模式挺好，出站 IP 带国旗也有必要，合一张卡看看。
/// 高度与 `getWidgetHeight(1)` 对齐，可放进任意 span 的栅格单元。
class OutboundSummary extends ConsumerWidget {
  const OutboundSummary({super.key});

  static String _emoji(String code) {
    final upper = code.toUpperCase();
    if (upper.length != 2) {
      return code;
    }
    return String.fromCharCode(upper.codeUnitAt(0) - 0x41 + 0x1F1E6) +
        String.fromCharCode(upper.codeUnitAt(1) - 0x41 + 0x1F1E6);
  }

  void _handleChangeMode(Mode mode) {
    globalState.container.read(setupActionProvider.notifier).changeMode(mode);
  }

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final mode = ref.watch(
      patchClashConfigProvider.select((state) => state.mode),
    );
    final detection = ref.watch(networkDetectionProvider);
    final ipInfo = detection.ipInfo;
    final scheme = context.colorScheme;
    final textTheme = context.textTheme;
    final thumbColor = switch (mode) {
      Mode.rule => scheme.secondaryContainer,
      Mode.global => globalState.theme.darken3PrimaryContainer,
      Mode.direct => scheme.tertiaryContainer,
    };
    return SizedBox(
      height: getWidgetHeight(1),
      child: CommonCard(
        child: Padding(
          padding: const EdgeInsets.symmetric(horizontal: 12, vertical: 8),
          child: Row(
            children: [
              Expanded(
                flex: 3,
                child: SizedBox(
                  height: 34,
                  child: CommonTabBar<Mode>(
                    children: Map.fromEntries(
                      Mode.values.map(
                        (item) => MapEntry(
                          item,
                          Center(
                            child: Text(
                              Intl.message(item.name),
                              maxLines: 1,
                              overflow: TextOverflow.ellipsis,
                              style: textTheme.titleSmall?.copyWith(
                                color: item == mode
                                    ? scheme.onSecondaryContainer
                                    : scheme.onSurfaceVariant,
                              ),
                            ),
                          ),
                        ),
                      ),
                    ),
                    padding: EdgeInsets.zero,
                    groupValue: mode,
                    onValueChanged: (value) {
                      if (value != null) {
                        _handleChangeMode(value);
                      }
                    },
                    thumbColor: thumbColor,
                  ),
                ),
              ),
              Container(
                width: 1,
                height: 28,
                margin: const EdgeInsets.symmetric(horizontal: 10),
                color: scheme.outlineVariant.withAlpha(120),
              ),
              Expanded(
                flex: 2,
                child: Row(
                  children: [
                    if (ipInfo != null)
                      Text(
                        _emoji(ipInfo.countryCode),
                        style: textTheme.titleMedium?.copyWith(
                          fontFamily: FontFamily.twEmoji.value,
                        ),
                      )
                    else
                      Icon(
                        Icons.public,
                        size: 16,
                        color: scheme.onSurfaceVariant,
                      ),
                    const SizedBox(width: 6),
                    Expanded(
                      child: Text(
                        ipInfo?.ip ?? (detection.isLoading ? '…' : '—'),
                        maxLines: 1,
                        overflow: TextOverflow.ellipsis,
                        style: textTheme.bodySmall?.copyWith(
                          color: scheme.onSurfaceVariant,
                        ),
                      ),
                    ),
                  ],
                ),
              ),
            ],
          ),
        ),
      ),
    );
  }
}
