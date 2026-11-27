import 'dart:async';
import 'dart:convert';
import 'dart:io';

import 'package:archive/archive_io.dart';
import 'package:drift/drift.dart';
import 'package:drift_flutter/drift_flutter.dart';
import 'package:fl_clash/common/common.dart';
import 'package:fl_clash/database/database.dart';
import 'package:fl_clash/enum/enum.dart';
import 'package:fl_clash/models/models.dart';
import 'package:flutter/foundation.dart';
import 'package:flutter/services.dart';
import 'package:path/path.dart';

Future<T> decodeJSONTask<T>(String data) async {
  return compute<String, T>(_decodeJSON, data);
}

Future<T> _decodeJSON<T>(String content) async {
  return json.decode(content);
}

Future<String> encodeJSONTask<T>(T data) async {
  return compute<T, String>(_encodeJSON, data);
}

Future<String> _encodeJSON<T>(T content) async {
  return json.encode(content);
}

Future<String> encodeYamlTask<T>(T data) async {
  return compute<T, String>(_encodeYaml, data);
}

Future<String> _encodeYaml<T>(T content) async {
  return yaml.encode(content);
}

Future<String> encodeMD5Task(String data) async {
  return compute<String, String>(_encodeMD5, data);
}

Future<String> _encodeMD5<T>(String content) async {
  return content.toMd5();
}

Future<List<Group>> toGroupsTask(ComputeGroupsState data) async {
  return compute<ComputeGroupsState, List<Group>>(_toGroupsTask, data);
}

Future<List<Group>> _toGroupsTask(ComputeGroupsState state) async {
  final proxiesData = state.proxiesData;
  final all = proxiesData.all;
  final sortType = state.sortType;
  final delayMap = state.delayMap;
  final selectedMap = state.selectedMap;
  final defaultTestUrl = state.defaultTestUrl;
  final proxies = proxiesData.proxies;
  if (proxies.isEmpty) return [];
  final groupsRaw = all
      .where((name) {
        final proxy = proxies[name] ?? {};
        return GroupTypeExtension.valueList.contains(proxy['type']);
      })
      .map((groupName) {
        final group = proxies[groupName];
        group['all'] = ((group['all'] ?? []) as List)
            .map((name) => proxies[name])
            .where((proxy) => proxy != null)
            .toList();
        return group;
      })
      .toList();
  final groups = groupsRaw.map((e) => Group.fromJson(e)).toList();
  return computeSort(
    groups: groups,
    sortType: sortType,
    delayMap: delayMap,
    selectedMap: selectedMap,
    defaultTestUrl: defaultTestUrl,
  );
}

Future<VM2<String, String>> makeRealProfileTask(
  MakeRealProfileState data,
) async {
  return compute<MakeRealProfileState, VM2<String, String>>(
    _makeRealProfileTask,
    data,
  );
}

Future<VM2<String, String>> _makeRealProfileTask(
  MakeRealProfileState data,
) async {
  final rawConfig = Map.from(data.rawConfig);
  final realPatchConfig = data.realPatchConfig;
  final profilesPath = data.profilesPath;
  final profileId = data.profileId;
  final overrideDns = data.overrideDns;
  final addedRules = data.addedRules;
  final appendSystemDns = data.appendSystemDns;
  final dnsGuard = data.dnsGuard;
  final defaultUA = data.defaultUA;
  String getProvidersFilePathInner(String type, String url) {
    return join(
      profilesPath,
      'providers',
      profileId.toString(),
      type,
      url.toMd5(),
    );
  }

  rawConfig['external-controller'] = realPatchConfig.externalController.value;
  rawConfig['external-ui'] = '';
  rawConfig['interface-name'] = '';
  rawConfig['external-ui-url'] = '';
  rawConfig['tcp-concurrent'] = realPatchConfig.tcpConcurrent;
  rawConfig['unified-delay'] = realPatchConfig.unifiedDelay;
  rawConfig['ipv6'] = realPatchConfig.ipv6;
  rawConfig['log-level'] = realPatchConfig.logLevel.name;
  rawConfig['port'] = 0;
  rawConfig['socks-port'] = 0;
  rawConfig['keep-alive-interval'] = realPatchConfig.keepAliveInterval;
  rawConfig['mixed-port'] = realPatchConfig.mixedPort;
  rawConfig['port'] = realPatchConfig.port;
  rawConfig['socks-port'] = realPatchConfig.socksPort;
  rawConfig['redir-port'] = realPatchConfig.redirPort;
  rawConfig['tproxy-port'] = realPatchConfig.tproxyPort;
  rawConfig['find-process-mode'] = realPatchConfig.findProcessMode.name;
  rawConfig['allow-lan'] = realPatchConfig.allowLan;
  rawConfig['mode'] = realPatchConfig.mode.name;
  if (rawConfig['tun'] == null) {
    rawConfig['tun'] = {};
  }
  rawConfig['tun']['enable'] = realPatchConfig.tun.enable;
  rawConfig['tun']['device'] = realPatchConfig.tun.device;
  rawConfig['tun']['dns-hijack'] = realPatchConfig.tun.dnsHijack;
  rawConfig['tun']['stack'] = realPatchConfig.tun.stack.name;
  rawConfig['tun']['route-address'] = realPatchConfig.tun.routeAddress;
  rawConfig['tun']['auto-route'] = realPatchConfig.tun.autoRoute;
  rawConfig['geodata-loader'] = realPatchConfig.geodataLoader.name;
  // 2026-11-27：默认开启 sniffer。内核 sniffer.enable 默认 false，后果是对
  // 「IP 直连 / HTTPDNS」的连接，GeoSite / DOMAIN 类规则全部失效，只能按 GEOIP
  // 判定 → 分流不准。订阅自带 sniffer 且已启用时不动它。
  if (rawConfig['sniffer']?['enable'] != true) {
    final merged = defaultSnifferConfig();
    final profileSniff = rawConfig['sniffer']?['sniff'];
    if (profileSniff is Map) {
      // 订阅已经写明 sniff 类型/端口 → 尊重订阅，只把 enable 与顶层开关补上。
      merged['sniff'] = <String, dynamic>{
        ...merged['sniff'] as Map<String, dynamic>,
      }..addAll(Map<String, dynamic>.from(profileSniff));
    }
    rawConfig['sniffer'] = merged;
  }
  if (rawConfig['sniffer']?['sniff'] != null) {
    for (final value in (rawConfig['sniffer']?['sniff'] as Map).values) {
      if (value['ports'] != null && value['ports'] is List) {
        value['ports'] =
            value['ports']?.map((item) => item.toString()).toList() ?? [];
      }
    }
  }
  if (rawConfig['profile'] == null) {
    rawConfig['profile'] = {};
  }
  if (rawConfig['proxy-providers'] != null) {
    final proxyProviders = rawConfig['proxy-providers'] as Map;
    for (final key in proxyProviders.keys) {
      final proxyProvider = proxyProviders[key];
      if (proxyProvider['type'] != 'http') {
        continue;
      }
      if (proxyProvider['url'] != null) {
        proxyProvider['path'] = getProvidersFilePathInner(
          'proxies',
          proxyProvider['url'],
        );
      }
    }
  }
  if (rawConfig['rule-providers'] != null) {
    final ruleProviders = rawConfig['rule-providers'] as Map;
    for (final key in ruleProviders.keys) {
      final ruleProvider = ruleProviders[key];
      if (ruleProvider['type'] != 'http') {
        continue;
      }
      if (ruleProvider['url'] != null) {
        ruleProvider['path'] = getProvidersFilePathInner(
          'rules',
          ruleProvider['url'],
        );
      }
    }
  }
  rawConfig['profile']['store-selected'] = false;
  rawConfig['geox-url'] = realPatchConfig.geoXUrl.raw;
  rawConfig['global-ua'] = realPatchConfig.globalUa ?? defaultUA;
  if (rawConfig['hosts'] == null) {
    rawConfig['hosts'] = {};
  }
  for (final host in realPatchConfig.hosts.entries) {
    rawConfig['hosts'][host.key] = host.value.splitByMultipleSeparators;
  }
  if (rawConfig['dns'] == null) {
    rawConfig['dns'] = {};
  }
  final isEnableDns = rawConfig['dns']['enable'] == true;
  const systemDns = 'system://';
  // 2026-11-27 DNS 审计（面向未知用户的产品默认值）：
  //  ① system:// 指向物理网的运营商 DNS（明文、可被劫持）。它一旦进 nameserver，
  //     会因 mihomo 的竞速机制（dns/util.go batchExchange 取最快的有效响应）几乎必胜，
  //     等于所有未被 nameserver-policy 命中的域名都走运营商明文；改为一律只进 fallback 兵底。
  //  ② 模板 fallback 原是 tls://8.8.4.4 / 1.1.1.1。主解析超时（换网/弱网必然发生）时
  //     mihomo 直接采用 fallback 结果（dns/resolver.go ipExchange）→ 国内域名拿到海外 IP。
  //     默认已改为 ['system://']，境外兵底降为用户可选。
  //  ③ dnsGuard（默认开）只做「外科手术」：从订阅 dns 里摘掉被判定为境外的 DNS 条目，
  //     其余字段（fake-ip-filter / proxy-server-nameserver / policy …）原样保留。
  //     判定只依据已知境外公共 DNS 清单，不在清单里的不动 —— 判断不了就不出手。
  if (overrideDns || !isEnableDns) {
    final dns = realPatchConfig.dns;
    // 转成 Map<String, dynamic>：freezed 的 toJson() 是强类型 Map，直接写 List 会抛
    // type 'List<dynamic>' is not a subtype of type 'List<String>'（在 compute isolate
    // 里抛出会让整个配置生成失败、VPN 起不来）。
    rawConfig['dns'] = <String, dynamic>{
      ...(dns.toJson() as Map).cast<String, dynamic>(),
    };
    rawConfig['dns']['nameserver-policy'] = {};
    for (final entry in dns.nameserverPolicy.entries) {
      rawConfig['dns']['nameserver-policy'][entry.key] =
          entry.value.splitByMultipleSeparators;
    }
  } else {
    rawConfig['dns'] = <String, dynamic>{
      ...(rawConfig['dns'] as Map).cast<String, dynamic>(),
    };
    if (dnsGuard) {
      _stripOverseasDnsEntries(
        rawConfig['dns'] as Map,
        templateNameserver: realPatchConfig.dns.nameserver,
        templateFallback: realPatchConfig.dns.fallback,
        templateBootNameserver: realPatchConfig.dns.defaultNameserver,
      );
    }
    // 订阅没写的键，原本会由 mihomo 内核默认值顶替（enhanced-mode=redir-host、
    // fake-ip-filter 只有 3 条 msft 域名……）。那些位置不是用户的选择，是我们该补的缺口：
    // 用模板默认补全，但绝不覆盖订阅已经写了的键。
    _fillDnsDefaults(rawConfig['dns'] as Map, realPatchConfig.dns);
    // 保留订阅 dns。内核默认 default-nameserver 含 8.8.8.8 / 1.0.0.1，
    // 订阅没写这个键时引导解析会走境外明文，必须补国内条目。
    final defaultNameserver = rawConfig['dns']['default-nameserver'];
    if (defaultNameserver is! List || defaultNameserver.isEmpty) {
      rawConfig['dns']['default-nameserver'] = List<String>.from(
        realPatchConfig.dns.defaultNameserver,
      );
    }
  }
  if (appendSystemDns) {
    // system:// 只追加到 fallback（兵底），不再进 nameserver。
    final List<String> fallback = List<String>.from(
      rawConfig['dns']['fallback'] ?? [],
    );
    if (!fallback.contains(systemDns)) {
      rawConfig['dns']['fallback'] = [...fallback, systemDns];
    }
  }
  List<String> rules = [];
  if (data.rules.isEmpty) {
    if (rawConfig['rules'] != null) {
      rules = List<String>.from(rawConfig['rules']);
    }
    if (addedRules.isNotEmpty) {
      final hasMatchPlaceholder = addedRules.any(
        (item) => item.ruleTarget?.toUpperCase() == 'MATCH',
      );
      String? replacementTarget;

      if (hasMatchPlaceholder) {
        for (int i = rules.length - 1; i >= 0; i--) {
          final parsed = Rule.parse(rules[i]);
          if (parsed.ruleAction == RuleAction.MATCH) {
            final target = parsed.ruleTarget;
            if (target != null && target.isNotEmpty) {
              replacementTarget = target;
              break;
            }
          }
        }
      }
      final List<String> finalAddedRules;

      if (replacementTarget?.isNotEmpty == true) {
        finalAddedRules = [];
        for (int i = 0; i < addedRules.length; i++) {
          final parsed = addedRules[i];
          if (parsed.ruleTarget?.toUpperCase() == 'MATCH') {
            finalAddedRules.add(
              parsed.copyWith(ruleTarget: replacementTarget).rawValue,
            );
          } else {
            finalAddedRules.add(addedRules[i].rawValue);
          }
        }
      } else {
        finalAddedRules = addedRules.map((e) => e.rawValue).toList();
      }
      rules = [...finalAddedRules, ...rules];
    }
  } else {
    rules = data.rules.map((item) => item.rawValue).toList();
  }
  if (data.proxyGroups.isNotEmpty) {
    rawConfig['proxy-groups'] = data.proxyGroups;
  }
  rawConfig['rules'] = rules;
  final yaml = await _encodeYaml(Map<String, dynamic>.from(rawConfig));
  return VM2(yaml, yaml.toMd5());
}

Future<List<String>> shakingProfileTask(
  VM2<Iterable<int>, Iterable<int>> data,
) async {
  return compute<
    VM3<Iterable<int>, Iterable<int>, RootIsolateToken>,
    List<String>
  >(_shakingProfileTask, VM3(data.a, data.b, RootIsolateToken.instance!));
}

Future<List<String>> _shakingProfileTask(
  VM3<Iterable<int>, Iterable<int>, RootIsolateToken> data,
) async {
  final profileIds = data.a;
  final scriptIds = data.b;
  final token = data.c;
  BackgroundIsolateBinaryMessenger.ensureInitialized(token);
  final profilesDir = Directory(await appPath.profilesPath);
  final scriptsDir = Directory(await appPath.scriptsDirPath);
  final providersDir = Directory(await appPath.getProvidersRootPath());
  final List<String> targets = [];
  void scanDirectory(
    Directory dir,
    Iterable<int> baseNames, {
    bool skipProvidersFolder = false,
  }) {
    if (!dir.existsSync()) return;
    final entities = dir.listSync(recursive: false, followLinks: false);

    for (final entity in entities) {
      if (entity is File) {
        final id = basenameWithoutExtension(entity.path);
        if (!baseNames.contains(int.tryParse(id))) {
          targets.add(entity.path);
        }
      } else if (skipProvidersFolder && entity is Directory) {
        if (basename(entity.path) == 'providers') {
          continue;
        }
      }
    }
  }

  scanDirectory(profilesDir, profileIds, skipProvidersFolder: true);
  scanDirectory(providersDir, profileIds);
  scanDirectory(scriptsDir, scriptIds);
  return targets;
}

Future<String> encodeLogsTask(List<Log> data) async {
  return compute<List<Log>, String>(_encodeLogsTask, data);
}

Future<String> _encodeLogsTask(List<Log> data) async {
  final logsRaw = data.map((item) => item.toString());
  final logsRawString = logsRaw.join('\n');
  return logsRawString;
}

Future<MigrationData> oldToNowTask(Map<String, Object?> data) async {
  final homeDir = await appPath.homeDirPath;
  return compute<VM3<Map<String, Object?>, String, String>, MigrationData>(
    _oldToNowTask,
    VM3(data, homeDir, homeDir),
  );
}

Future<MigrationData> _oldToNowTask(
  VM3<Map<String, Object?>, String, String> data,
) async {
  final configMap = data.a;
  final sourcePath = data.b;
  final targetPath = data.c;

  final accessControlMap = configMap['accessControl'];
  final isAccessControl = configMap['isAccessControl'];
  if (accessControlMap != null) {
    (accessControlMap as Map)['enable'] = isAccessControl;
    if (configMap['vpnProps'] != null) {
      final vpnPropsRaw = configMap['vpnProps'] as Map;
      vpnPropsRaw['accessControl'] = accessControlMap;
    }
  }
  if (configMap['vpnProps'] != null) {
    final vpnPropsRaw = configMap['vpnProps'] as Map;
    vpnPropsRaw['accessControlProps'] = vpnPropsRaw['accessControl'];
  }
  configMap['davProps'] = configMap['dav'];
  final appSettingProps =
      configMap['appSetting'] as Map<String, dynamic>? ?? {};
  appSettingProps['restoreStrategy'] = appSettingProps['recoveryStrategy'];
  configMap['appSettingProps'] = appSettingProps;
  configMap['proxiesStyleProps'] = configMap['proxiesStyle'];
  configMap['proxiesStyleProps'] = configMap['proxiesStyle'];
  // final overwriteMap = configMap['overwrite'] as Map? ?? {};
  // configMap['overwriteType'] = overwriteMap['type'];
  // configMap['scriptId'] = overwriteMap['scriptOverwrite'];
  List rawScripts = configMap['scripts'] as List<dynamic>? ?? [];
  if (rawScripts.isEmpty) {
    final scriptPropsJson = configMap['scriptProps'] as Map<String, dynamic>?;
    if (scriptPropsJson != null) {
      rawScripts = scriptPropsJson['scripts'] as List<dynamic>? ?? [];
    }
  }
  final Map<String, int> idMap = {};
  final List<Script> scripts = [];
  for (final rawScript in rawScripts) {
    final id = rawScript['id'] as String?;
    final content = rawScript['content'] as String?;
    final label = rawScript['label'] as String?;
    if (id == null || content == null || label == null) {
      continue;
    }
    final newId = idMap.updateCacheValue(rawScript['id'], () => snowflake.id);
    final path = _getScriptPath(targetPath, newId.toString());
    final file = File(path);
    await file.safeWriteAsString(content);
    scripts.add(
      Script(id: newId, label: label, lastUpdateTime: DateTime.now()),
    );
  }
  final List rawRules = configMap['rules'] as List<dynamic>? ?? [];
  final List<Rule> rules = [];
  final List<ProfileRuleLink> links = [];
  for (final rawRule in rawRules) {
    final id = idMap.updateCacheValue(rawRule['id'], () => snowflake.id);
    rawRule['id'] = id;
    final value = rawRule['value'] ?? '';
    rules.add(Rule.parse(value, id: id));
    links.add(ProfileRuleLink(ruleId: id));
  }
  final List rawProfiles = configMap['profiles'] as List<dynamic>? ?? [];
  final List<Profile> profiles = [];
  for (final rawProfile in rawProfiles) {
    final rawId = rawProfile['id'] as String?;
    if (rawId == null) {
      continue;
    }
    final profileId = idMap.updateCacheValue(rawId, () => snowflake.id);
    rawProfile['id'] = profileId;
    final overwrite = rawProfile['overwrite'] as Map?;
    if (overwrite != null) {
      final standardOverwrite = overwrite['standardOverwrite'] as Map?;
      if (standardOverwrite != null) {
        final addedRules = standardOverwrite['addedRules'] as List? ?? [];
        for (final addRule in addedRules) {
          final id = idMap.updateCacheValue(addRule['id'], () => snowflake.id);
          final value = addRule['value'] ?? '';
          rules.add(Rule.parse(value, id: id));
          links.add(
            ProfileRuleLink(
              profileId: profileId,
              ruleId: id,
              scene: RuleScene.added,
            ),
          );
        }
        final disabledRuleIds = standardOverwrite['disabledRuleIds'] as List?;
        if (disabledRuleIds != null) {
          for (final disabledRuleId in disabledRuleIds) {
            final newDisabledRuleId = idMap[disabledRuleId];
            if (newDisabledRuleId != null) {
              links.add(
                ProfileRuleLink(
                  profileId: profileId,
                  ruleId: newDisabledRuleId,
                  scene: RuleScene.disabled,
                ),
              );
            }
          }
        }
      }
      final scriptOverwrite = overwrite['scriptOverwrite'] as Map?;
      if (scriptOverwrite != null) {
        final scriptId = scriptOverwrite['scriptId'] as String?;
        rawProfile['scriptId'] = scriptId != null ? idMap[scriptId] : null;
      }
      rawProfile['overwriteType'] = overwrite['type'];
    }

    final sourceFile = File(_getProfilePath(sourcePath, rawId));
    final targetFilePath = _getProfilePath(targetPath, profileId.toString());
    await sourceFile.safeCopy(targetFilePath);
    profiles.add(Profile.fromJson(rawProfile));
  }
  final currentProfileId = configMap['currentProfileId'];
  configMap['currentProfileId'] = currentProfileId != null
      ? idMap[currentProfileId]
      : null;
  return MigrationData(
    configMap: configMap,
    profiles: profiles,
    rules: rules,
    scripts: scripts,
    links: links,
  );
}

Future<String> backupTask(
  Map<String, dynamic> configMap,
  Iterable<String> fileNames,
) async {
  return compute<
    VM3<Map<String, dynamic>, Iterable<String>, RootIsolateToken>,
    String
  >(_backupTask, VM3(configMap, fileNames, RootIsolateToken.instance!));
}

Future<String> _backupTask<T>(
  VM3<Map<String, dynamic>, Iterable<String>, RootIsolateToken> args,
) async {
  final configMap = args.a;
  final fileNames = args.b;
  final token = args.c;
  BackgroundIsolateBinaryMessenger.ensureInitialized(token);
  final dbPath = await appPath.databasePath;
  final configStr = json.encode(configMap);
  final profilesDir = Directory(await appPath.profilesPath);
  final scriptsDir = Directory(await appPath.scriptsDirPath);
  final tempZipFilePath = await appPath.tempFilePath;
  final tempDBFile = File(await appPath.tempFilePath);
  final tempConfigFile = File(await appPath.tempFilePath);
  final dbFile = File(dbPath);
  if (await dbFile.exists()) {
    await dbFile.copy(tempDBFile.path);
  }
  final encoder = ZipFileEncoder();
  encoder.create(tempZipFilePath);
  await tempConfigFile.writeAsString(configStr);
  await encoder.addFile(tempDBFile, backupDatabaseName);
  await encoder.addFile(tempConfigFile, configJsonName);
  if (await profilesDir.exists()) {
    await encoder.addDirectory(
      profilesDir,
      filter: (file, _) {
        if (!fileNames.contains(basename(file.path))) {
          return ZipFileOperation.skip;
        }
        return ZipFileOperation.include;
      },
    );
  }
  if (await scriptsDir.exists()) {
    await encoder.addDirectory(
      scriptsDir,
      filter: (file, _) {
        if (!fileNames.contains(basename(file.path))) {
          return ZipFileOperation.skip;
        }
        return ZipFileOperation.include;
      },
    );
  }
  // ZeroTier 身份：不在 profiles/scripts 里，但丢了节点地址就变了（要重新授权）。
  final ztIdentityFile = File(join(await appPath.homeDirPath, ztIdentityFileName));
  if (await ztIdentityFile.exists()) {
    await encoder.addFile(ztIdentityFile, ztIdentityFileName);
  }
  // UI 布局（ui.json）：丢了只回落内置布局，不影响功能；顺手带上免得重导。
  final uiSchemaFile = File(join(await appPath.homeDirPath, 'ui.json'));
  if (await uiSchemaFile.exists()) {
    await encoder.addFile(uiSchemaFile, 'ui.json');
  }
  encoder.close();
  await tempConfigFile.safeDelete();
  await tempDBFile.safeDelete();
  return tempZipFilePath;
}

Future<MigrationData> restoreTask() async {
  return compute<RootIsolateToken, MigrationData>(
    _restoreTask,
    RootIsolateToken.instance!,
  );
}

Future<MigrationData> _restoreTask(RootIsolateToken token) async {
  BackgroundIsolateBinaryMessenger.ensureInitialized(token);
  final backupFilePath = await appPath.backupFilePath;
  final restoreDirPath = await appPath.restoreDirPath;
  final homeDirPath = await appPath.homeDirPath;
  final zipDecoder = ZipDecoder();
  final input = InputFileStream(backupFilePath);
  final archive = zipDecoder.decodeStream(input);
  final dir = Directory(restoreDirPath);
  await dir.create(recursive: true);
  for (final file in archive.files) {
    final outPath = join(restoreDirPath, posix.normalize(file.name));
    final outputStream = OutputFileStream(outPath);
    file.writeContent(outputStream);
    await outputStream.close();
  }
  await input.close();
  final restoreConfigFile = File(join(restoreDirPath, configJsonName));
  if (!await restoreConfigFile.exists()) {
    throw currentAppLocalizations.invalidBackupFile;
  }
  final restoreConfigMap =
      json.decode(await restoreConfigFile.readAsString())
          as Map<String, Object?>?;
  final version = restoreConfigMap?['version'] ?? 0;
  MigrationData migrationData = MigrationData(configMap: restoreConfigMap);
  if (version == 0 && restoreConfigMap != null) {
    migrationData = await _oldToNowTask(
      VM3(restoreConfigMap, restoreDirPath, homeDirPath),
    );
    return migrationData;
  }
  final backupDatabaseFile = File(join(restoreDirPath, backupDatabaseName));
  if (!await backupDatabaseFile.exists()) {
    return migrationData;
  }
  final database = Database(
    driftDatabase(
      name: 'database',
      native: DriftNativeOptions(
        databaseDirectory: () async => Directory(restoreDirPath),
      ),
    ),
  );
  final results = await Future.wait([
    database.profilesDao.query().get(),
    database.scriptsDao.query().get(),
    database.rules.all().map((item) => item.toRule()).get(),
    database.profileRuleLinks.all().map((item) => item.toLink()).get(),
    database.proxyGroups.all().map((item) => item.toProxyGroup()).get(),
  ]);
  final profiles = results[0].cast<Profile>();
  final scripts = results[1].cast<Script>();
  final profilesMigration = profiles.map(
    (item) => VM2(
      _getProfilePath(restoreDirPath, item.id.toString()),
      _getProfilePath(homeDirPath, item.id.toString()),
    ),
  );
  final scriptsMigration = scripts.map(
    (item) => VM2(
      _getScriptPath(restoreDirPath, item.id.toString()),
      _getScriptPath(homeDirPath, item.id.toString()),
    ),
  );
  // ZeroTier 身份：旧备份里没这个文件，存在就写回（覆盖当前 identity）。
  final restoredIdentity = File(join(restoreDirPath, ztIdentityFileName));
  final identityMigration = <VM2<String, String>>[];
  if (await restoredIdentity.exists()) {
    identityMigration.add(
      VM2(restoredIdentity.path, join(homeDirPath, ztIdentityFileName)),
    );
  }
  await _copyWithMapList([
    ...profilesMigration,
    ...scriptsMigration,
    ...identityMigration,
  ]);
  migrationData = migrationData.copyWith(
    profiles: profiles,
    scripts: scripts,
    rules: results[2].cast<Rule>(),
    links: results[3].cast<ProfileRuleLink>(),
    proxyGroups: results[4].cast<ProxyGroup>(),
  );
  await database.close();
  return migrationData;
}

Future<void> _copyWithMapList(List<VM2<String, String>> copyMapList) async {
  await Future.wait(
    copyMapList.map((item) => File(item.a).safeCopy(item.b)).toList(),
  );
}

String _getScriptPath(String root, String fileName) {
  return join(root, 'scripts', '$fileName.js');
}

String _getProfilePath(String root, String fileName) {
  return join(root, 'profiles', '$fileName.yaml');
}

Future<List<T>> mapListTask<T, S>(List<S> results, T Function(S) mapper) async {
  return compute<VM2<List<S>, T Function(S)>, List<T>>(
    _mapListTask,
    VM2(results, mapper),
  );
}

Future<List<T>> _mapListTask<T, S>(VM2<List<S>, T Function(S)> vm2) async {
  final results = vm2.a;
  final mapper = vm2.b;
  return results.map((item) => mapper(item)).toList();
}

/// 默认 sniffer 配置。
///
/// 内核默认 sniffer.enable=false → 域名类规则对 IP 直连/HTTPDNS 连接全部无效。
/// override-destination 让嗅探到的域名真正参与路由（含重新解析）。
/// skip-domain 放不适用覆盖的：Apple 推送需要真实 IP，被覆盖会推不到。
Map<String, dynamic> defaultSnifferConfig() => <String, dynamic>{
      'enable': true,
      'force-dns-mapping': true,
      'parse-pure-ip': true,
      // 2026-11-27 复审：只让嗅探结果参与规则匹配，绝不改写连接目标。
      // 源码依据：component/sniffer/dispatcher.go replaceDomain() 只有 overrideDest 为真时
      // 才 Host=嗅探域名 、DstIP 置空（= 按新域名重连）；而 constant/metadata.go RuleHost()
      // 保证 DOMAIN/SUFFIX/KEYWORD/REGEX/GEOSITE/rule-provider 在 false 时照样按嗅探域名命中。
      // 所以关掉只损失「改写」，不损失分流准确性，也避免误嗅探把连接导错。
      'override-destination': false,
      'sniff': <String, dynamic>{
        'HTTP': <String, dynamic>{
          'ports': ['80', '8080-8880'],
          'override-destination': false,
        },
        'TLS': <String, dynamic>{
          'ports': ['443', '8443'],
        },
        'QUIC': <String, dynamic>{
          'ports': ['443', '8443'],
        },
      },
      'skip-domain': ['+.push.apple.com'],
      // 私网/回环目的地址不嗅探：内网服务（打印机、NAS、路由管理页）没什么可嗅的，
      // 白读几个包又没收益。不吃掉这些流量，只是不做无谓的解析尝试。
      'skip-dst-address': [
        '127.0.0.0/8',
        '10.0.0.0/8',
        '172.16.0.0/12',
        '192.168.0.0/16',
        '169.254.0.0/16',
        '::1/128',
        'fc00::/7',
        'fe80::/10',
      ],
    };

/// 已知境外公共 DNS（DoH/DoT/明文）。
/// App 侧没有 GeoIP 数据，只能用清单判断「订阅是不是把解析交给了境外服务器」
/// —— 那会把国内域名解析到海外 CDN（产品红线）。命中后由 dnsGuard 开关决定是否覆写。
const _overseasDnsHosts = <String>[
  'dns.google',
  'dns.google.com',
  'cloudflare-dns.com',
  'one.one.one.one',
  'mozilla.cloudflare-dns.com',
  'dns.quad9.net',
  'dns9.quad9.net',
  'dns10.quad9.net',
  'dns11.quad9.net',
  'doh.opendns.com',
  'dns.adguard.com',
  'dns.adguard-dns.com',
  'dns.nextdns.io',
  'doh.dns.sb',
  'dns.sb',
  'dns.mullvad.net',
  'doh.cleanbrowsing.org',
];

/// 境外公共 DNS 的 IPv4 前缀（8.8.8.8 / 1.1.1.1 / 9.9.9.9 / 208.67.222.222 …）。
const _overseasDnsIpPrefixes = <String>[
  '8.8.',
  '1.1.1.',
  '1.0.0.',
  '9.9.9.',
  '149.112.',
  '208.67.',
  '94.140.',
  '76.76.',
  '45.90.',
  '185.228.',
  '162.159.',
];

String _dnsHostOf(String server) {
  var value = server.trim().toLowerCase();
  final schemeIndex = value.indexOf('://');
  if (schemeIndex >= 0) {
    value = value.substring(schemeIndex + 3);
  }
  final slashIndex = value.indexOf('/');
  if (slashIndex >= 0) {
    value = value.substring(0, slashIndex);
  }
  final hashIndex = value.indexOf('#');
  if (hashIndex >= 0) {
    value = value.substring(0, hashIndex);
  }
  if (value.startsWith('[')) {
    final close = value.indexOf(']');
    if (close > 0) {
      value = value.substring(1, close);
    }
  } else if (value.contains(':')) {
    value = value.split(':').first;
  }
  return value;
}

bool _isOverseasDnsServer(String server) {
  final host = _dnsHostOf(server);
  if (host.isEmpty || host == 'system') {
    return false;
  }
  for (final known in _overseasDnsHosts) {
    if (host == known || host.endsWith('.$known')) {
      return true;
    }
  }
  for (final prefix in _overseasDnsIpPrefixes) {
    if (host.startsWith(prefix)) {
      return true;
    }
  }
  return false;
}

/// 用模板默认补全订阅 dns 里**没写**的键。
///
/// 判据：这些位置原本由 mihomo 内核默认值顶替（redir-host / 3 条 msft fake-ip-filter /
/// 含 8.8.8.8 的 default-nameserver），不是用户的选择。补缺口是纯收益，覆盖才是干预。
void _fillDnsDefaults(Map dns, Dns template) {
  final templateJson = template.toJson();
  const keys = <String>[
    'enhanced-mode',
    'fake-ip-range',
    'fake-ip-range6',
    'fake-ip-filter',
    'use-hosts',
    'use-system-hosts',
    'respect-rules',
    'ipv6',
  ];
  for (final key in keys) {
    if (dns[key] == null) {
      dns[key] = templateJson[key];
    }
  }
  final templatePolicy = template.nameserverPolicy;
  if (templatePolicy.isEmpty) {
    return;
  }
  final existing = dns['nameserver-policy'];
  if (existing is Map) {
    templatePolicy.forEach((key, value) {
      if (!existing.containsKey(key)) {
        existing[key] = value.splitByMultipleSeparators;
      }
    });
    return;
  }
  final rebuilt = <String, dynamic>{};
  templatePolicy.forEach((key, value) {
    rebuilt[key] = value.splitByMultipleSeparators;
  });
  dns['nameserver-policy'] = rebuilt;
}

/// 把订阅 dns 里被判定为境外的 DNS 条目摘掉（外科手术式，其余字段不动）。
///
/// 只在 dnsGuard 打开时调用。判定只依据已知境外公共 DNS 清单（域名 + IP 前缀），
/// 不在清单里的条目一律保留 —— 判断不了就不出手。
void _stripOverseasDnsEntries(
  Map dns, {
  required List<String> templateNameserver,
  required List<String> templateFallback,
  required List<String> templateBootNameserver,
}) {
  List<String> keepAll(dynamic value) {
    final out = <String>[];
    if (value is List) {
      for (final item in value) {
        if (item is String && item.isNotEmpty && !_isOverseasDnsServer(item)) {
          out.add(item);
        }
      }
    } else if (value is String &&
        value.isNotEmpty &&
        !_isOverseasDnsServer(value)) {
      out.add(value);
    }
    return out;
  }

  for (final key in const <String>[
    'nameserver',
    'fallback',
    'default-nameserver',
    'proxy-server-nameserver',
  ]) {
    if (!dns.containsKey(key)) {
      continue;
    }
    dns[key] = keepAll(dns[key]);
  }

  final policy = dns['nameserver-policy'];
  if (policy is Map) {
    final rebuilt = <String, dynamic>{};
    policy.forEach((key, value) {
      if (key is! String) {
        return;
      }
      if (value is String) {
        final parts = value
            .split(RegExp(r'[, ]+'))
            .where((part) => part.isNotEmpty)
            .where((part) => !_isOverseasDnsServer(part))
            .toList();
        rebuilt[key] = parts.isEmpty
            ? 'system://'
            : (parts.length == 1 ? parts.first : parts);
      } else {
        final kept = keepAll(value);
        rebuilt[key] = kept.isEmpty ? 'system://' : kept;
      }
    });
    dns['nameserver-policy'] = rebuilt;
  }

  // 摘完之后不能留空，否则这份 dns 块整块不能用。
  final nameserver = dns['nameserver'];
  if (nameserver is! List || nameserver.isEmpty) {
    dns['nameserver'] = List<String>.from(templateNameserver);
  }
  final fallback = dns['fallback'];
  if (fallback is! List || fallback.isEmpty) {
    dns['fallback'] = List<String>.from(templateFallback);
  }
  final bootNameserver = dns['default-nameserver'];
  if (bootNameserver is List && bootNameserver.isEmpty) {
    dns['default-nameserver'] = List<String>.from(templateBootNameserver);
  }
}
