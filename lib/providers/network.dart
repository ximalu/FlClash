import 'package:fl_clash/models/network_state.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

/// 原型阶段：调试用状态场景。接真数据后连同 NetworkDemoScene 一起删除。
class NetworkDemoSceneController extends Notifier<NetworkDemoScene> {
  @override
  NetworkDemoScene build() => NetworkDemoScene.s1;

  void select(NetworkDemoScene scene) {
    state = scene;
  }
}

final networkDemoSceneProvider =
    NotifierProvider<NetworkDemoSceneController, NetworkDemoScene>(
  NetworkDemoSceneController.new,
);

/// 页面唯一数据源。
///
/// 当前返回演示数据；接真数据时**只改本 provider 的函数体**：
/// `VpnStatus` ← CoreStatus，`ExitState`/`LinkHealth`/`peers` ← GetRuntimeStatus
/// + ZT_Node_peers，`probes` ← 探测器。视图层与模型不变（设计文档 P4）。
final networkStateProvider = Provider<NetworkState>((ref) {
  final scene = ref.watch(networkDemoSceneProvider);
  return demoNetworkState(scene);
});
