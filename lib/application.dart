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
              if (!first) _scheduleZeroTierColdStart(transports);
            }
          },
          child: child,
        ),
      ),
    );
  }

  /// 换网后的**冷启动优先**策略（0.9.28 试验版）：先等网络稳定 2.5 s，然后
  /// **直接重建整个 ZT Node** —— 新 node 没有任何历史路径，等于人为制造一次冷启动。
  ///
  /// 依据（2026-11-27 用户实测）：Dev 0.9.1 冷启动 20 s 就连上 ZT、i3 曲库能放；
  /// 而换网走「换 socket + 软探测」实测 44–83 s，甚至几分钟。差别不在 socket，
  /// 而在**对端路径内存**：换网前节点和 i3 之间有一条看起来还有效的旧路径，
  /// 于是它继续往死路发、不做 rendezvous，直到 ZT 内部路径过期（~243 s）。
  ///
  /// 2.5 s 去抖是必要的：一次切网 Android 会连报两三张网（2026-11-27 实机
  /// 25 s 内 5 次 rebind），每个事件都冷启动只会互相抵消。
  void _scheduleZeroTierColdStart(Set<ConnectivityResult> transports) {
    if (!system.isAndroid) return;
    if (!ref.read(isStartProvider)) return;
    _ztRebindTimer?.cancel();
    _ztRebindRetryTimer?.cancel();
    _ztRebindAttempt = 0;
    commonPrint.log('[ZT-COLD] detect transports=$transports at ${DateTime.now().toIso8601String()} (debounce 2.5s)');
    _ztRebindTimer = Timer(const Duration(milliseconds: 2500), _zeroTierColdStart);
  }

  /// 一步到位的冷启动。被 Go 侧限速（20 s 内已经重启过）时才退化成换 socket。
  /// 之后保留旧阶梯做兜底：+12 s 仍无回帧就继续换 socket（第 5 步会再冷启动）。
  Future<void> _zeroTierColdStart() async {
    if (!ref.read(isStartProvider)) return;
    final sw = Stopwatch()..start();
    final restarted = await _restartZeroTierNode();
    commonPrint.log(
      '[ZT-COLD] network change -> node restart=$restarted (${sw.elapsedMilliseconds}ms)',
    );
    if (!restarted) {
      await _rebuildZeroTierSocket('cold start rate-limited');
    }
    _ztRebindRetryTimer?.cancel();
    _ztRebindRetryTimer = Timer(const Duration(seconds: 12), _ztRebindFallbackStep);
  }

  /// 冷启动之后的兜底入口：确实还没回帧才继续走老阶梯（从换 socket 开始）。
  Future<void> _ztRebindFallbackStep() async {
    if (!ref.read(isStartProvider)) return;
    if (!await _zeroTierFramesStillFrozen(minMs: 4000)) return;
    _ztRebindAttempt = 1;
    await _ztRebindStep();
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
