// 纯逻辑单测：探测目标校验 + 健康四态判据（不碰 Riverpod / 网络 / UI）。
import 'package:fl_clash/models/network_state.dart';
import 'package:flutter_test/flutter_test.dart';

ProbeResult _result({
  required int successes,
  required int total,
  int consecutiveFailures = 0,
  int consecutiveSuccesses = 0,
  bool? ok,
}) {
  return ProbeResult(
    ok: ok ?? consecutiveFailures == 0,
    consecutiveFailures: consecutiveFailures,
    consecutiveSuccesses: consecutiveSuccesses,
    successCount: successes,
    windowCount: total,
  );
}

void main() {
  group('isValidProbeHost（第一版只收 IP）', () {
    test('接受 IPv4 / IPv6 字面量', () {
      expect(isValidProbeHost('192.168.196.81'), isTrue);
      expect(isValidProbeHost('10.1.2.3'), isTrue);
      expect(isValidProbeHost('::1'), isTrue);
      expect(isValidProbeHost('fe80::1'), isTrue);
    });

    test('拒绝域名与垃圾输入', () {
      expect(isValidProbeHost('localhost'), isFalse);
      expect(isValidProbeHost('nas.lan'), isFalse);
      expect(isValidProbeHost(''), isFalse);
      expect(isValidProbeHost('   '), isFalse);
    });
  });

  group('deriveProbeHealth 四态 + 迟滞', () {
    test('样本不足 → unknown（不拿 1 次结果冒充健康）', () {
      expect(
        deriveProbeHealth(ProbeHealth.unknown, _result(successes: 1, total: 1)),
        ProbeHealth.unknown,
      );
      expect(
        deriveProbeHealth(
          ProbeHealth.unknown,
          _result(successes: 2, total: 2, consecutiveSuccesses: 2),
        ),
        ProbeHealth.unknown,
      );
    });

    test('证据够了才进 ok：连续 3 成功且成功率 >= 90%', () {
      expect(
        deriveProbeHealth(
          ProbeHealth.unknown,
          _result(successes: 3, total: 3, consecutiveSuccesses: 3),
        ),
        ProbeHealth.ok,
      );
    });

    test('ok 时单次、两次失败不改色', () {
      expect(
        deriveProbeHealth(
          ProbeHealth.ok,
          _result(successes: 19, total: 20, consecutiveFailures: 1, ok: false),
        ),
        ProbeHealth.ok,
      );
      expect(
        deriveProbeHealth(
          ProbeHealth.ok,
          _result(successes: 18, total: 20, consecutiveFailures: 2, ok: false),
        ),
        ProbeHealth.ok,
      );
    });

    test('连续 3 次失败 → down', () {
      expect(
        deriveProbeHealth(
          ProbeHealth.ok,
          _result(successes: 17, total: 20, consecutiveFailures: 3, ok: false),
        ),
        ProbeHealth.down,
      );
    });

    test('间歇性故障不再被迟滞掩盖：20 次里 10 败 → down', () {
      // 既不构成连续 3 败，也不到 90% 成功率（旧逻辑会一直停在绿色）。
      expect(
        deriveProbeHealth(
          ProbeHealth.ok,
          _result(successes: 10, total: 20, consecutiveFailures: 1, ok: false),
        ),
        ProbeHealth.down,
      );
    });

    test('20 次里 4–9 败 → degraded（可见但不判死）', () {
      for (final fails in [4, 6, 9]) {
        expect(
          deriveProbeHealth(
            ProbeHealth.ok,
            _result(successes: 20 - fails, total: 20),
          ),
          ProbeHealth.degraded,
          reason: 'fails=$fails 应降级',
        );
      }
      expect(
        deriveProbeHealth(
          ProbeHealth.unknown,
          _result(successes: 15, total: 20, consecutiveSuccesses: 1),
        ),
        ProbeHealth.degraded,
      );
    });

    test('down 需要连续 3 成功且成功率 >= 95% 才回 ok；90% 只回到 degraded', () {
      expect(
        deriveProbeHealth(
          ProbeHealth.down,
          _result(successes: 1, total: 20, consecutiveSuccesses: 1),
        ),
        ProbeHealth.down,
      );
      expect(
        deriveProbeHealth(
          ProbeHealth.down,
          _result(successes: 18, total: 20, consecutiveSuccesses: 3),
        ),
        ProbeHealth.degraded,
      );
      expect(
        deriveProbeHealth(
          ProbeHealth.down,
          _result(successes: 19, total: 20, consecutiveSuccesses: 3),
        ),
        ProbeHealth.ok,
      );
    });

    test('degraded 回 ok 也要 95%（比保档 90% 严，避免临界抖动）', () {
      expect(
        deriveProbeHealth(
          ProbeHealth.degraded,
          _result(successes: 18, total: 20, consecutiveSuccesses: 3),
        ),
        ProbeHealth.degraded,
      );
      expect(
        deriveProbeHealth(
          ProbeHealth.degraded,
          _result(successes: 19, total: 20, consecutiveSuccesses: 3),
        ),
        ProbeHealth.ok,
      );
    });

    test('无结果 / 还没跑过 → unknown', () {
      expect(
        deriveProbeHealth(ProbeHealth.ok, null),
        ProbeHealth.unknown,
      );
    });

    test('观测中断 → unknown（不拿中断前的成功当健康）', () {
      expect(
        deriveProbeHealth(
          ProbeHealth.ok,
          _result(successes: 20, total: 20, consecutiveSuccesses: 20),
          observationGap: true,
        ),
        ProbeHealth.unknown,
      );
      expect(
        deriveProbeHealth(
          ProbeHealth.down,
          _result(successes: 0, total: 20, consecutiveFailures: 20, ok: false),
          observationGap: true,
        ),
        ProbeHealth.unknown,
      );
    });
  });
}
