// 纯逻辑单测：探测目标校验 + 颜色迟逆（不碰 Riverpod / 网络 / UI）。
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

  group('deriveProbeHealth 迟逆', () {
    test('首次成功 → ok，首次失败只到 degraded（不直接判死）', () {
      expect(
        deriveProbeHealth(ProbeHealth.unknown, _result(successes: 1, total: 1)),
        ProbeHealth.ok,
      );
      expect(
        deriveProbeHealth(
          ProbeHealth.unknown,
          _result(
            successes: 0,
            total: 1,
            consecutiveFailures: 1,
            ok: false,
          ),
        ),
        ProbeHealth.degraded,
      );
    });

    test('单次（甚至 2 次）失败不改色', () {
      expect(
        deriveProbeHealth(
          ProbeHealth.ok,
          _result(
            successes: 19,
            total: 20,
            consecutiveFailures: 1,
            ok: false,
          ),
        ),
        ProbeHealth.ok,
      );
      expect(
        deriveProbeHealth(
          ProbeHealth.ok,
          _result(
            successes: 18,
            total: 20,
            consecutiveFailures: 2,
            ok: false,
          ),
        ),
        ProbeHealth.ok,
      );
    });

    test('连续 3 次失败 → down', () {
      expect(
        deriveProbeHealth(
          ProbeHealth.ok,
          _result(
            successes: 17,
            total: 20,
            consecutiveFailures: 3,
            ok: false,
          ),
        ),
        ProbeHealth.down,
      );
    });

    test('down 需要 3 次连续成功 + 成功率 >= 90% 才回到 ok', () {
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
          _result(successes: 8, total: 20, consecutiveSuccesses: 3),
        ),
        ProbeHealth.down,
      );
      expect(
        deriveProbeHealth(
          ProbeHealth.down,
          _result(successes: 19, total: 20, consecutiveSuccesses: 3),
        ),
        ProbeHealth.ok,
      );
    });

    test('无结果 → unknown', () {
      expect(
        deriveProbeHealth(ProbeHealth.ok, null),
        ProbeHealth.unknown,
      );
    });
  });
}
