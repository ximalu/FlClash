<div>

[**简体中文**](README_zh_CN.md)

</div>

# FlClashTier

A fork of [FlClash](https://github.com/chen08209/FlClash) that runs a proxy (mihomo)
**and** a ZeroTier private network inside a single Android `VpnService` slot.

- **Source code lives in this repository.**
- **APK downloads and design docs:** [ximalu/FlClashTier](https://github.com/ximalu/FlClashTier)

## What this fork adds

- Embedded ZeroTier core 1.16.2 inside the same TUN — no second VPN service, no
  fighting over the single VPN slot.
- Managed-route based flow routing: destinations in a ZeroTier managed route go to
  the ZT core, everything else goes to mihomo. Routes come from the core, never
  hard-coded.
- Physical path self-healing: the ZT UDP socket is bound to the **current** physical
  network (`Network.bindSocket`), rebuilt on network change, peer paths are reset so a
  new hole punch starts immediately, escalating to a real node rebuild if that fails.
- Datapath diagnostics: layered counters (wire rx → core → frames → TUN), sendto errno
  histogram, `zt-diag.log` / `app-diag.log` attached to the exported log.
- 网络 (Network) page: service state, exits, TCP probes, peer table and diagnostics,
  including live ZT RX/TX rates.

## Build

Requirements: Flutter 3.47+, Go 1.26, Android NDK r28c.

```bash
export ANDROID_HOME=$HOME/Android/Sdk
export ANDROID_NDK=$HOME/Android/Sdk/ndk/28.2.13676358
export GOPROXY=https://goproxy.cn,direct
make core-android ARCH=arm64
export PATH=$HOME/flutter/bin:$PATH
flutter build apk --release --target-platform android-arm64
```

- `--target-platform android-arm64` is mandatory: only arm64 ships a ZeroTier static
  library (32-bit arm fails at link time).
- The ZeroTier static library lives in `core/zerotier/lib/`. It is built from
  ZeroTierOne 1.16.2 plus a small patch that exports `ZT_Node_resetPeerPaths`.
- The prebuilt C++ wrapper (`core/zerotier/cpp_src/wrapper.cpp`) is compiled by the
  build tool; after changing it, rebuild with `go build -a` (the Go cache does not
  track external object files).

## Known limitations

- arm64 only.
- Needs the system VPN slot (as every `VpnService` app does).
- The peer table cannot show direct/relay: the ZeroTier C API gives no reliable
  evidence, so the app shows path counts instead of guessing.
- The first network switch after a cold start can take up to ~90 s to bring the ZT
  path back; later switches are usually unnoticeable.

## License and credits

GPL-3.0, same as upstream. Built on [FlClash](https://github.com/chen08209/FlClash) by
chen08209, with [mihomo / Clash.Meta](https://github.com/MetaCubeX/mihomo) as the proxy
core and [ZeroTierOne](https://github.com/zerotier/ZeroTierOne) (BSL 1.1) as the
virtual network core.
