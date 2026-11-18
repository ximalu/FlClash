import 'dart:io';

/// 业务探测的一次尝试结果（瞬时，不持久化）。
///
/// 语义边界：**TCP 建连成功只证明该端口可从本机连上**，不代表应用层业务正常。
/// UI 文案只能说「从我这里可连通」，不得写「服务正常 / 在线」。
class ProbeAttempt {
  final bool ok;
  final int? rttMs;

  const ProbeAttempt({required this.ok, this.rttMs});
}

const Duration kProbeTimeout = Duration(seconds: 2);

/// 对一个 TCP 端点做一次建连探测。
///
/// 不抛异常：超时 / 拒绝 / 不可达全部归约为 `ok = false`。
/// 连接成功立即 destroy（我们只关心能不能建连，不读也不写）。
Future<ProbeAttempt> tcpConnectProbe(
  String host,
  int port, {
  Duration timeout = kProbeTimeout,
}) async {
  Socket? socket;
  var ok = false;
  final stopwatch = Stopwatch()..start();
  try {
    socket = await Socket.connect(host, port, timeout: timeout);
    ok = true;
  } catch (_) {
    ok = false;
  } finally {
    try {
      socket?.destroy();
    } catch (_) {}
    stopwatch.stop();
  }
  return ProbeAttempt(ok: ok, rttMs: ok ? stopwatch.elapsedMilliseconds : null);
}
