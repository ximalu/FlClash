#!/usr/bin/env bash
# 发版前自检：l10n 占位符必须写成 Dart 插值 ${x}，不能是字面量 {x}。
# 背景（台账 #13）：手写 l10n 时把 intl/arb 的 {a}/{b} 原样写进 Dart 字符串，
# 结果「接收 {a} · 发送 {b}」直接上屏，4 个 locale 全中，用户看不到数字。
# 命中即 exit 1；在 flutter build 之前跑。
# 注：l10n.dart 的 /// 文档注释会引用模板原文（合法的 {a}），必须排除；
#     代码里的正确写法是 ${a}，负向环视 (?<!\$) 把 ${a} 放过。
set -uo pipefail
cd "$(dirname "$0")/.." || exit 2

hits=$(grep -rnP '(?<!\$)\{[a-zA-Z_][a-zA-Z0-9_]*\}' \
  lib/l10n/intl/messages_*.dart lib/l10n/l10n.dart 2>/dev/null \
  | grep -vP '^[^:]+:\d+:\s*//')

if [ -n "$hits" ]; then
  echo "FAIL: l10n 里存在字面量占位符（应写成 \${x}）："
  echo "$hits"
  exit 1
fi

echo "OK: l10n 占位符无字面量残留"
