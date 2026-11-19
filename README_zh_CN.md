<div>

[**English**](README.md)

</div>

## FlClash

## FlClashTier 分支

这个分支把代理（mihomo）和 ZeroTier 私有网络放进**同一个 Android VpnService**，
两条出口同时工作，不用装两个 VPN App 抢一个 VPN 位。上游没有的能力：

- 同一个 tun 内嵌 ZeroTier core（不占第二个 VPN Service）
- ZeroTier 状态卡（地址、网络、对端路径）
- 数据路径修复：专用 TUN writer goroutine + errno 分类、TCP MSS clamping 让段不超 ZT MTU、分层计数器
- 一键导出日志（会附带滚动的 `zt-datapath.log`）

构建履历

- `0.9.11+2026111904` **换网自愈做成分层三档**：`protect()` 的返回值打通整条 JNI 链，且 ZT 的 UDP socket 会 `Network.bindSocket` 绑到**当前物理网络**（不再只是「绕开自己的 TUN」）—— WiFi↔蜂窝切换后新建的 socket 才真的从活的那条路出去。换 socket 仍救不回来时，看护升级到**真正重建 ZeroTier Node**（同进程、同 TUN、同 flow router，`nodeGen` 1→2），而不是那条从不碰 node 的「重启核心」。升级链：冻结 20s→换 socket，45s→换 node（最多 2 次、间隔 120s），最后由 Dart 层在 150s 重启核心。测试 `TestG1..G5`、`TestJudgeFrameProgress`、`TestH_WatchdogEscalationChain`
- `0.9.2+2026111701` **ZeroTier 物理通道自愈**（`[ZT-SOCKET]`）：入站通道僵死时热替换 ZT 的 UDP socket，WiFi↔蜂窝切换后不再需要重启 App。看护线程的判据是「有出站、零入站持续 60 秒」（不是单纯零入站，避免熄屏空闲被误判为通道死亡）；node identity、网络成员状态、TUN 全部不动。回归测试 `TestG_SocketRebuild`。下载：https://github.com/ximalu/FlClash/releases/tag/v0.9.2%2B2026111701
- `0.9.1+2026111602` 首个**正式签名**构建（包名 `com.flclashtier`，RSA 4096）—— 下载：https://github.com/ximalu/FlClash/releases/tag/v0.9.1%2B2026111602
- `0.9.1+2026111601` 内嵌 ZT core 1.16.2、TUN writer + MSS clamping、分层计数、数据面日志文件（debug 签名，包名 `com.flclashtier.dev`）
- `0.9.0+2026090901` 首个诊断版（TUN 写重试 + 完整 EIO 上下文）
- `0.8.x` ZeroTier 状态卡、多语言、versionName 含 versionCode


[![Downloads](https://img.shields.io/github/downloads/chen08209/FlClash/total?style=flat-square&logo=github)](https://github.com/chen08209/FlClash/releases/)[![Last Version](https://img.shields.io/github/release/chen08209/FlClash/all.svg?style=flat-square)](https://github.com/chen08209/FlClash/releases/)[![License](https://img.shields.io/github/license/chen08209/FlClash?style=flat-square)](LICENSE)

[![Channel](https://img.shields.io/badge/Telegram-Channel-blue?style=flat-square&logo=telegram)](https://t.me/FlClash)

基于ClashMeta的多平台代理客户端，简单易用，开源无广告。

on Desktop:
<p style="text-align: center;">
    <img alt="desktop" src="snapshots/desktop.gif">
</p>

on Mobile:
<p style="text-align: center;">
    <img alt="mobile" src="snapshots/mobile.gif">
</p>

## Features

✈️ 多平台: Android, Windows, macOS and Linux

💻 自适应多个屏幕尺寸,多种颜色主题可供选择

💡 基本 Material You 设计, 类[Surfboard](https://github.com/getsurfboard/surfboard)用户界面

☁️ 支持通过WebDAV同步数据

✨ 支持一键导入订阅, 深色模式

## Use

### Linux

⚠️ 使用前请确保安装以下依赖

   ```bash
    sudo apt-get install libayatana-appindicator3-dev
    sudo apt-get install libkeybinder-3.0-dev
   ```

### Android

支持下列操作

   ```bash
    com.follow.clash.action.START
    
    com.follow.clash.action.STOP
    
    com.follow.clash.action.TOGGLE
   ```

## Download

<a href="https://chen08209.github.io/FlClash-fdroid-repo/repo?fingerprint=789D6D32668712EF7672F9E58DEEB15FBD6DCEEC5AE7A4371EA72F2AAE8A12FD"><img alt="Get it on F-Droid" src="snapshots/get-it-on-fdroid.svg" width="200px"/></a> <a href="https://github.com/chen08209/FlClash/releases"><img alt="Get it on GitHub" src="snapshots/get-it-on-github.svg" width="200px"/></a>

### Homebrew

```bash
brew tap chen08209/tap
brew install --cask flclash
```

## Build

1. 更新 submodules
   ```bash
   git submodule update --init --recursive
   ```

2. 安装 `Flutter` 以及 `Golang` 环境

3. 构建应用

    - android

        1. 安装  `Android SDK` ,  `Android NDK`

        2. 设置 `ANDROID_NDK` 环境变量

        3. 运行构建脚本

           ```bash
           dart setup.dart android
           ```

    - windows

        1. 你需要一个windows客户端

        2. 安装 `GCC`，`Inno Setup`

        3. 运行构建脚本

           ```bash
           dart setup.dart windows
           ```

    - linux

        1. 你需要一个linux客户端

        2. 依赖会由 setup 脚本自动安装，也可以手动安装：
           ```bash
           sudo apt-get install -y libayatana-appindicator3-dev libkeybinder-3.0-dev
           ```

        3. 运行构建脚本

           ```bash
           dart setup.dart linux
           ```

    - macOS

        1. 你需要一个macOS客户端

        2. 运行构建脚本

           ```bash
           dart setup.dart macos
           ```

## Star

支持开发者的最简单方式是点击页面顶部的星标（⭐）。

<p style="text-align: center;">
    <a href="https://api.star-history.com/svg?repos=chen08209/FlClash&Date">
        <img alt="start" width=50% src="https://api.star-history.com/svg?repos=chen08209/FlClash&Date"/>
    </a>
</p>
