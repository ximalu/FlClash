import 'dart:async';
import 'dart:io';

import 'package:connectivity_plus/connectivity_plus.dart';
import 'package:fl_clash/common/common.dart';
import 'package:fl_clash/core/core.dart';
import 'package:fl_clash/l10n/l10n.dart';
import 'package:fl_clash/manager/hotkey_manager.dart';
import 'package:fl_clash/manager/manager.dart';
import 'package:fl_clash/plugins/app.dart';
import 'package:fl_clash/providers/providers.dart';
import 'package:fl_clash/state.dart';
import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_localizations/flutter_localizations.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import 'pages/pages.dart';

class Application extends ConsumerStatefulWidget {
  const Application({super.key});

  @override
  ConsumerState<Application> createState() => ApplicationState();
}

class ApplicationState extends ConsumerState<Application> {
  Timer? _autoUpdateProfilesTaskTimer;
  Timer? _ztRebindTimer;
  Timer? _ztRebindRetryTimer;
  int _ztRebindAttempt = 0;
  bool _preHasVpn = false;
  Set<ConnectivityResult>? _preTransports;

  final _pageTransitionsTheme = const PageTransitionsTheme(
    builders: <TargetPlatform, PageTransitionsBuilder>{
      TargetPlatform.android: commonSharedXPageTransitions,
      TargetPlatform.windows: commonSharedXPageTransitions,
      TargetPlatform.linux: commonSharedXPageTransitions,
      TargetPlatform.macOS: commonSharedXPageTransitions,
    },
  );

  ColorScheme _getAppColorScheme({
    required Brightness brightness,
    int? primaryColor,
  }) {
    return ref.read(genColorSchemeProvider(brightness));
  }

  @override
  void initState() {
    super.initState();
    SystemNavigator.setFrameworkHandlesBack(true);
    WidgetsBinding.instance.addPostFrameCallback((timeStamp) async {
      if (globalState.navigatorKey.currentContext != null) {
        await globalState.attach();
      } else {
        exit(0);
      }
      _autoUpdateProfilesTask();
      _initLink();
      app?.initShortcuts();
    });
  }

  void _initLink() {
    linkManager.initAppLinksListen((url) async {
      final res = await globalState.showMessage(
        title: currentAppLocalizations.addProfile,
        message: TextSpan(
          children: [
            TextSpan(text: currentAppLocalizations.doYouWantToPass),
            TextSpan(
              text: ' $url ',
              style: TextStyle(
                color: context.colorScheme.primary,
                decoration: TextDecoration.underline,
                decorationColor: context.colorScheme.primary,
              ),
            ),
            TextSpan(text: currentAppLocalizations.createProfile),
          ],
        ),
      );
      if (res != true) return;
      ref.read(profilesActionProvider.notifier).addProfileFormURL(url);
    });
  }

  void _autoUpdateProfilesTask() {
    _autoUpdateProfilesTaskTimer = Timer(const Duration(minutes: 20), () async {
      await ref.read(profilesActionProvider.notifier).autoUpdateProfiles();
      _autoUpdateProfilesTask();
    });
  }

  Widget _buildPlatformState({required Widget child}) {
    if (system.isDesktop) {
      return WindowManager(
        child: TrayManager(
          child: HotKeyManager(child: ProxyManager(child: child)),
        ),
      );
    }
    return AndroidManager(child: TileManager(child: child));
  }

  Widget _buildState({required Widget child}) {
    return AppStateManager(
      child: CoreManager(
        child: ConnectivityManager(
          onConnectivityChanged: (results) async {
            commonPrint.log('connectivityChanged ${results.toString()}');
            ref.read(systemActionProvider.notifier).updateLocalIp();
            final hasVpn = results.contains(ConnectivityResult.vpn);
            if (_preHasVpn == hasVpn) {
              ref.read(checkIpNumProvider.notifier).add();
            }
            _preHasVpn = hasVpn;
            // 只认真实传输的变化（去掉 vpn 这一项）：VPN 自己起停不是换网。
            final transports = results
                .where((result) => result != ConnectivityResult.vpn)
                .toSet();
            final previous = _preTransports;
            final sameTransports = previous != null &&
                transports.length == previous.length &&
                transports.containsAll(previous);
            if (!sameTransports) {
              final first = previous == null;
              _preTransports = transports;
              if (!first) _scheduleZeroTierSocketRebuild(transports);
            }
          },
          child: child,
        ),
      ),
    );
  }

  /// 换网恢复入口（0.9.29 回到阶梯路线）。
  ///
  /// 冷启动路线已被实测否定（2026-11-27 凌晨，用 0.9.1 dev 做「杀进程 → 切网 →
  /// 冷启动」对照，连做两次）：**WiFi 方向 10 s 内恢复，蜂窝方向 3 分钟以上且
  /// dnat 始终为 0**。真冷启动（新 node、无任何历史路径、重新 join）在蜂窝上
  /// 同样救不回来 —— 说明瓶颈不在 node 状态，而在**蜂窝侧的路径建立**
  /// （CGNAT / 可能没有可用 IPv6），不是靠清状态能绕过的东西。
  ///
  /// 所以恢复原来的阶梯：T+2 s 换 socket + 软探测；10/18/26 s 重试；34 s 升级。
  /// 首试等 2 s 而不是立即：Android 换网后默认网络往往还没就绪（2026-11-19 实机）。
  void _scheduleZeroTierSocketRebuild(Set<ConnectivityResult> transports) {
    if (!system.isAndroid) return;
    if (!ref.read(isStartProvider)) return;
    _ztRebindTimer?.cancel();
    _ztRebindRetryTimer?.cancel();
    _ztRebindAttempt = 0;
    commonPrint.log('[ZT-NET] transport change $transports -> ladder in 2s');
    _ztRebindTimer = Timer(const Duration(seconds: 2), _ztRebindStep);
  }

  /// 换网自愈阶梯（0.9.18）：
  ///   T+2s  换 socket + 重置 peer 路径（0.9.15 起 preferred）
  ///   T+10/18/26s  仍冻结（>5s 无回帧）→ 再换一次 socket + 重置
  ///   T+34s  仍冻结（>20s）→ 重建 ZT node（Go 侧 45s 限速，被限速则再换 socket）
  ///
  /// 为什么要重试而不是一次定生死（2026-11-26 首次切蜂窝 88s）：手机关掉
  /// WiFi 后要 30s 左右才真正 attach 上蜂窝，2s 那次 protect+bind 绑到的是
  /// 过渡网络，reset 的 HELLO 发不出去 -> 对端侧 paths=0，只能干等。把重试
  /// 窗口覆盖前 30 秒，才卡在新的 NAT 映射刚建立的那一刻重新打洞。
  Future<void> _ztRebindStep() async {
    if (!ref.read(isStartProvider)) return;
    _ztRebindAttempt++;
    final attempt = _ztRebindAttempt;
    if (attempt == 1) {
      await _rebuildZeroTierSocket('after connectivity change');
    } else if (attempt <= 4) {
      // 早期重试只看“确实没有回帧”（>5s），不用 20s 判死阀——那个阀是给
      // node 重建档位用的，拿它卡早期重试会让第一次重试永远不执行。
      if (!await _zeroTierFramesStillFrozen(minMs: 5000)) return;
      await _rebuildZeroTierSocket('reset retry #$attempt');
    } else {
      if (!await _zeroTierFramesStillFrozen()) return;
      final restarted = await _restartZeroTierNode();
      if (!restarted) {
        await _rebuildZeroTierSocket('node restart limited, socket retry');
      }
      return;
    }
    _ztRebindRetryTimer?.cancel();
    _ztRebindRetryTimer = Timer(const Duration(seconds: 8), _ztRebindStep);
  }

  Future<bool> _restartZeroTierNode() async {
    if (!ref.read(isStartProvider)) return false;
    try {
      final ok = await coreController.restartZeroTierNode();
      commonPrint.log('[ZT] node restart (connectivity change): $ok');
      return ok;
    } catch (_) {
      return false;
    }
  }

  Future<void> _rebuildZeroTierSocket(String reason) async {
    if (!ref.read(isStartProvider)) return;
    final ok = await coreController.rebuildZeroTierSocket();
    commonPrint.log('[ZT] socket rebuild ($reason): $ok');
    ref.read(checkIpNumProvider.notifier).add();
  }

  /// 数据面是否仍然冻结。默认 20s（Go 侧判死阀，用于升级到 node 重建）；
  /// 早期 socket 重试传 5s——只要确实没有回帧就该再打一次洞。
  Future<bool> _zeroTierFramesStillFrozen({int minMs = 20000}) async {
    try {
      final status = await coreController.getZeroTierStatus();
      final frozen = status['framesFrozenMs'];
      return frozen is int && frozen >= minMs;
    } catch (_) {
      return false;
    }
  }

  Widget _buildPlatformApp({required Widget child}) {
    if (system.isDesktop) {
      return WindowHeaderContainer(child: child);
    }
    return VpnManager(child: child);
  }

  Widget _buildApp({required Widget child}) {
    return StatusManager(child: ThemeManager(child: child));
  }

  @override
  Widget build(context) {
    return Consumer(
      builder: (_, ref, child) {
        final locale = ref.watch(
          appSettingProvider.select((state) => state.locale),
        );
        final themeProps = ref.watch(themeSettingProvider);
        return MaterialApp(
          debugShowCheckedModeBanner: false,
          navigatorKey: globalState.navigatorKey,
          onNavigationNotification: (_) => true,
          localizationsDelegates: const [
            AppLocalizations.delegate,
            GlobalMaterialLocalizations.delegate,
            GlobalCupertinoLocalizations.delegate,
            GlobalWidgetsLocalizations.delegate,
          ],
          builder: (_, child) {
            return AppEnvManager(
              child: _buildApp(
                child: _buildPlatformState(
                  child: _buildState(child: _buildPlatformApp(child: child!)),
                ),
              ),
            );
          },
          scrollBehavior: BaseScrollBehavior(),
          title: appName,
          locale: utils.getLocaleForString(locale),
          supportedLocales: AppLocalizations.delegate.supportedLocales,
          themeMode: themeProps.themeMode,
          theme: ThemeData(
            useMaterial3: true,
            pageTransitionsTheme: _pageTransitionsTheme,
            colorScheme: _getAppColorScheme(
              brightness: Brightness.light,
              primaryColor: themeProps.primaryColor,
            ),
          ),
          darkTheme: ThemeData(
            useMaterial3: true,
            pageTransitionsTheme: _pageTransitionsTheme,
            colorScheme: _getAppColorScheme(
              brightness: Brightness.dark,
              primaryColor: themeProps.primaryColor,
            ).toPureBlack(themeProps.pureBlack),
          ),
          home: child!,
        );
      },
      child: const HomePage(),
    );
  }

  @override
  void dispose() {
    linkManager.destroy();
    _autoUpdateProfilesTaskTimer?.cancel();
    _ztRebindTimer?.cancel();
    super.dispose();
  }
}
