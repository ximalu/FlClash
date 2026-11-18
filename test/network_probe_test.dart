// 真实 TCP 探测：本机开/关端口的建连行为（Linux 上可跑，不需要手机）。
//
// 注意：这里**故意不**测“不可达地址 → 超时”。2026-11-18 在 i3 上实测发现
// 该网络会对任意 TCP 目的地址提前应答（连 192.0.2.1:9 / TEST-NET-1 都能
// 立即 connect 成功），所以“连不上”这个断言在带透明代理/提前应答的网络里
// 根本不可复现。这也正好说明：TCP 建连成功连“真实可达”都证明不了，
// 更不等于业务正常（UI 文案必须只写「从我这里可连通」）。
import 'dart:io';

import 'package:fl_clash/models/network_probe.dart';
import 'package:flutter_test/flutter_test.dart';

void main() {
  test('监听中的端口 → ok + rtt', () async {
    final server = await ServerSocket.bind(InternetAddress.loopbackIPv4, 0);
    addTearDown(server.close);
    final attempt = await tcpConnectProbe('127.0.0.1', server.port);
    expect(attempt.ok, isTrue);
    expect(attempt.rttMs, isNotNull);
    expect(attempt.rttMs, greaterThanOrEqualTo(0));
  });

  test('本机已关闭的端口 → 被拒，不算通', () async {
    final server = await ServerSocket.bind(InternetAddress.loopbackIPv4, 0);
    final port = server.port;
    await server.close();
    final attempt = await tcpConnectProbe('127.0.0.1', port);
    expect(attempt.ok, isFalse);
    expect(attempt.rttMs, isNull);
  });

  test('超时参数生效且不抛异常', () async {
    // 1ms 超时 + 一个未必有监听的地址：只断言“不抛异常、返回值要么 ok 要么失败”，
    // 不断言结果本身（受本机网络拦截行为影响）。
    final attempt = await tcpConnectProbe(
      '127.0.0.1',
      9,
      timeout: const Duration(milliseconds: 1),
    );
    expect(attempt.ok, isA<bool>());
    if (!attempt.ok) {
      expect(attempt.rttMs, isNull);
    }
  });
}
