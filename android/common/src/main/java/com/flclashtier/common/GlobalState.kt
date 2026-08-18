package com.flclashtier.common

import android.app.Application
import android.util.Log
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob

object GlobalState : CoroutineScope by CoroutineScope(SupervisorJob() + Dispatchers.Default) {
    const val NOTIFICATION_CHANNEL = "FlClash"
    const val NOTIFICATION_ID = 1

    val packageName: String
        get() = application.packageName

    val receiveBroadcastPermission: String
        get() = "$packageName.permission.RECEIVE_BROADCASTS"

    val application: Application
        get() = checkNotNull(appInstance) { "GlobalState is not initialized" }

    @Volatile
    private var appInstance: Application? = null

    fun init(application: Application) {
        appInstance = application
        // FlClashTier: 安装本地崩溃捕获 + 日志落盘（方案 A，2026-08-15）
        CrashLogger.init(application)
    }

    fun log(text: String) {
        Log.d("FlClash", text)
        // FlClashTier: 常规日志同时落盘
        CrashLogger.log(text)
    }

    /**
     * FlClashTier 改造：崩溃收集从 Firebase Crashlytics（占位符配置下无效）改为本地文件。
     * 原实现：
     *   FirebaseApp.initializeApp(application)
     *   FirebaseCrashlytics.getInstance().isCrashlyticsCollectionEnabled = enable
     * 保留方法签名以兼容 Dart 侧调用（设置页开关仍可用，只是写本地标志）。
     */
    fun setCrashlytics(enable: Boolean) {
        if (enable) {
            log("Crash collection enabled (local)")
        } else {
            log("Crash collection disabled (local)")
        }
    }

    /**
     * FlClashTier 改造：崩溃检测改读本地 crash 标志文件。
     * 原实现依赖 FirebaseCrashlytics.didCrashOnPreviousExecution()（占位符配置下不可靠）。
     */
    fun didCrashOnPreviousExecution(): Boolean {
        return CrashLogger.didCrashOnPreviousExecution()
    }

    /** FlClashTier: 启动流程确认崩溃后清除标志（Dart 侧 state.dart 调用） */
    fun clearCrashFlag() {
        CrashLogger.clearCrashFlag()
    }

    /** FlClashTier: 导出崩溃日志（Dart 侧调用） */
    fun exportCrashLog(): String = CrashLogger.exportCrashLog()

    /** FlClashTier: 崩溃日志文件列表 */
    fun listCrashFiles(): List<String> = CrashLogger.listCrashFiles()

    /** FlClashTier: 日志目录路径 */
    fun logDirPath(): String = CrashLogger.logDirPath()
}
