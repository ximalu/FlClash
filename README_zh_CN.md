<div>

[**English**](README.md)

</div>

# FlClashTier

基于 [FlClash](https://github.com/chen08209/FlClash) 的二次开发版本：在**同一个**
Android `VpnService` 里同时运行代理（mihomo）和 ZeroTier 私有网络，不再抢那一个 VPN 槽位。

- **源码在本仓库。**
- **APK 下载与设计文档：** [ximalu/FlClashTier](https://github.com/ximalu/FlClashTier)

## 相比上游新增

- 嵌入式 ZeroTier core 1.16.2，与本机 TUN 共用同一条数据通道，不需要第二个 VPN 服务。
- 基于 Managed Routes 的分流：命中 ZT 网段的流量走 ZT core，其余走 mihomo。
  路由来自 core 下发，绝不硬编码网段。
- 物理路径自愈：ZT 的 UDP socket 绑定**当前**物理网络（`Network.bindSocket`），
  换网时重建 socket 并重置对端路径（立即重新打洞），失败再升级为重建 node。
- 数据面诊断：分层计数（wire rx → core → frames → TUN）、sendto errno 直方图、
  导出日志时附带 `zt-diag.log` / `app-diag.log`。
- 新增「网络」页：服务 / 出口 / TCP 探测 / 对端 / 诊断，含 ZT 实时收发速率。

## 构建

依赖：Flutter 3.47+、Go 1.26、Android NDK r28c。

```bash
export ANDROID_HOME=$HOME/Android/Sdk
export ANDROID_NDK=$HOME/Android/Sdk/ndk/28.2.13676358
export GOPROXY=https://goproxy.cn,direct
make core-android ARCH=arm64
export PATH=$HOME/flutter/bin:$PATH
flutter build apk --release --target-platform android-arm64
```

- 必须带 `--target-platform android-arm64`：只有 arm64 有 ZeroTier 静态库（32 位 arm 链接必然失败）。
- ZeroTier 静态库在 `core/zerotier/lib/`，由 ZeroTierOne 1.16.2 + 一处补丁（导出
  `ZT_Node_resetPeerPaths`）编译而来。
- 预编译的 C++ wrapper 由构建脚本编译；改动 `wrapper.cpp` 后必须 `go build -a`
  （Go 的构建缓存不跟踪外部 .o 文件内容）。

## 已知限制

- 仅 arm64。
- 需要系统 VPN 槽位（所有 `VpnService` 应用都一样）。
- 对端列表无法显示「直连 / 中继」：ZeroTier 的 C API 给不出可靠证据，界面改为显示路径数量。
- 冷启动后的第一次换网，ZT 通路可能需要约 90 秒才恢复；后续换网通常无感。

## 许可与致谢

GPL-3.0（与上游一致）。基于 [FlClash](https://github.com/chen08209/FlClash) 二次开发，
代理内核为 [mihomo / Clash.Meta](https://github.com/MetaCubeX/mihomo)，
虚拟网络内核为 [ZeroTierOne](https://github.com/zerotier/ZeroTierOne)（BSL 1.1）。
