// 纯逻辑单测：启动偏好判定 + 出口恢复选择（不碰 Riverpod / Android）。
import 'package:fl_clash/models/network_state.dart';
import 'package:flutter_test/flutter_test.dart';

void main() {
  group('shouldAutoStartVpn（启动偏好）', () {
    test('默认不自动开：restoreOnLaunch=false', () {
      expect(
        shouldAutoStartVpn(
          restoreOnLaunch: false,
          lastServiceOn: true,
          hasAnyExit: true,
          vpnRunning: false,
        ),
        isFalse,
      );
    });

    test('上次没真正起来（lastServiceOn=false）不自动开', () {
      expect(
        shouldAutoStartVpn(
          restoreOnLaunch: true,
          lastServiceOn: false,
          hasAnyExit: true,
          vpnRunning: false,
        ),
        isFalse,
      );
    });

    test('两个出口都关 → 不开 VPN（硬约束）', () {
      expect(
        shouldAutoStartVpn(
          restoreOnLaunch: true,
          lastServiceOn: true,
          hasAnyExit: false,
          vpnRunning: false,
        ),
        isFalse,
      );
    });

    test('已在运行时不重复启动', () {
      expect(
        shouldAutoStartVpn(
          restoreOnLaunch: true,
          lastServiceOn: true,
          hasAnyExit: true,
          vpnRunning: true,
        ),
        isFalse,
      );
    });

    test('三个条件都满足才自动开', () {
      expect(
        shouldAutoStartVpn(
          restoreOnLaunch: true,
          lastServiceOn: true,
          hasAnyExit: true,
          vpnRunning: false,
        ),
        isTrue,
      );
    });
  });

  group('resolveExitWhenAllOff（两出口全关时的恢复选择）', () {
    test('优先上次启用的出口', () {
      expect(resolveExitWhenAllOff('zerotier'), 'zerotier');
      expect(resolveExitWhenAllOff('mihomo'), 'mihomo');
    });

    test('没有记录 → 回到公网代理', () {
      expect(resolveExitWhenAllOff(null), 'mihomo');
      expect(resolveExitWhenAllOff(''), 'mihomo');
      expect(resolveExitWhenAllOff('garbage'), 'mihomo');
    });
  });
}
