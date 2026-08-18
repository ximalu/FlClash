package com.flclashtier.common

import android.content.Context
import android.os.Build
import android.os.Environment
import android.util.Log
import java.io.File
import java.io.PrintWriter
import java.io.StringWriter
import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale
import java.util.concurrent.Executors

/**
 * FlClashTier 本地崩溃日志收集（方案 A，2026-08-15）
 *
 * 目标：崩溃信息不再依赖 Firebase（占位符配置下上报不到任何地方），改为落盘到
 * 应用私有目录，用户/开发者可直接导出查看。
 *
 * 覆盖范围：
 *  - Java/Kotlin 层未捕获异常（Thread.setDefaultUncaughtExceptionHandler）
 *  - GlobalState.log() 的常规日志（滚动文件，保留最近 1MB）
 *  - 崩溃标志（didCrashOnPreviousExecution 改读本地文件，去掉 Firebase 依赖）
 *
 * 限制（已知）：
 *  - 抓不到 native 崩溃（libclash.so / libcore.so 内部 SIGSEGV），那是 C/Go 层，
 *    需要 breakpad 级别的 native handler（后续可评估）
 */
object CrashLogger {
    private const val TAG = "FlClashTierCrash"
    private const val LOG_DIR = "logs"
    private const val CRASH_FLAG = "crash_flag"
    private const val MAX_LOG_BYTES = 1024 * 1024 // 1MB 滚动上限

    @Volatile
    private var appContext: Context? = null

    private val executor = Executors.newSingleThreadExecutor()

    private val logDateFormat = SimpleDateFormat("yyyy-MM-dd HH:mm:ss.SSS", Locale.US)
    private val crashDateFormat = SimpleDateFormat("yyyyMMdd-HHmmss", Locale.US)

    private var originalHandler: Thread.UncaughtExceptionHandler? = null

    /** 在 Application.onCreate 调用（必须在任何线程可能崩溃前注册） */
    fun init(context: Context) {
        appContext = context.applicationContext
        // 保存原 handler，崩溃时先写日志再交给原 handler（或系统默认）
        originalHandler = Thread.getDefaultUncaughtExceptionHandler()
        Thread.setDefaultUncaughtExceptionHandler { thread, throwable ->
            writeCrashLog(thread, throwable)
            originalHandler?.uncaughtException(thread, throwable)
                ?: run {
                    // 没有原 handler 时自己结束进程（模拟默认行为）
                    android.os.Process.killProcess(android.os.Process.myPid())
                }
        }
        Log.d(TAG, "CrashLogger installed, previous handler: ${originalHandler?.javaClass?.name ?: "null"}")
    }

    /** 常规日志落盘（异步，不阻塞调用方） */
    fun log(text: String) {
        val ctx = appContext ?: return
        executor.execute {
            try {
                val dir = getLogDir(ctx) ?: return@execute
                val file = File(dir, "app.log")
                val line = "[${logDateFormat.format(Date())}] $text\n"
                // 追加写
                file.appendText(line)
                // 超限滚动：截断为一半（简单滚动，不轮转多文件）
                if (file.length() > MAX_LOG_BYTES) {
                    val content = file.readText()
                    file.writeText(content.takeLast(MAX_LOG_BYTES / 2))
                }
            } catch (_: Exception) {
                // 日志失败不影响主流程
            }
        }
    }

    /** 崩溃时写 crash 文件 + 置崩溃标志 */
    private fun writeCrashLog(thread: Thread, throwable: Throwable) {
        val ctx = appContext ?: return
        executor.execute {
            try {
                val dir = getLogDir(ctx) ?: return@execute
                val sw = StringWriter()
                val pw = PrintWriter(sw)
                pw.println("=== FlClashTier Crash Report ===")
                pw.println("Time: ${logDateFormat.format(Date())}")
                pw.println("Thread: ${thread.name} (${thread.id})")
                pw.println("Device: ${Build.MANUFACTURER} ${Build.MODEL}")
                pw.println("Android: ${Build.VERSION.RELEASE} (API ${Build.VERSION.SDK_INT})")
                pw.println("Package: ${ctx.packageName}")
                pw.println("=== Stack Trace ===")
                throwable.printStackTrace(pw)
                pw.flush()

                // 崩溃详情文件（时间戳命名，保留历史）
                val crashFile = File(dir, "crash_${crashDateFormat.format(Date())}.txt")
                crashFile.writeText(sw.toString())

                // 崩溃标志文件（下次启动检测用）
                File(dir, CRASH_FLAG).writeText(sw.toString())

                // 把最近 app.log 也复制一份进 crash 报告目录（便于看崩溃前上下文）
                val appLog = File(dir, "app.log")
                if (appLog.exists()) {
                    val contextCopy = File(dir, "crash_context_${crashDateFormat.format(Date())}.log")
                    contextCopy.writeText(appLog.readText().takeLast(200_000))
                }

                Log.d(TAG, "Crash written: ${crashFile.absolutePath}")
            } catch (e: Exception) {
                Log.e(TAG, "Failed to write crash log", e)
            }
        }
        // 给异步写盘一点时间（最多 2s），避免进程立刻被杀导致日志没落盘
        try {
            Thread.sleep(2000)
        } catch (_: InterruptedException) {
        }
    }

    /** 上次是否崩溃（读本地标志文件） */
    fun didCrashOnPreviousExecution(): Boolean {
        val ctx = appContext ?: return false
        val flag = File(getLogDir(ctx), CRASH_FLAG)
        return flag.exists()
    }

    /** 清除崩溃标志（启动流程确认后调用） */
    fun clearCrashFlag() {
        val ctx = appContext ?: return
        executor.execute {
            try {
                File(getLogDir(ctx), CRASH_FLAG).delete()
            } catch (_: Exception) {
            }
        }
    }

    /** 导出崩溃日志文本（Dart 侧调用，返回最近 crash 报告 + app.log 尾部） */
    fun exportCrashLog(): String {
        val ctx = appContext ?: return "CrashLogger not initialized"
        return try {
            val dir = getLogDir(ctx) ?: return "no log dir"
            val sb = StringBuilder()
            // 最近一个 crash 文件
            val crashFiles = dir.listFiles { f -> f.name.startsWith("crash_") && f.name.endsWith(".txt") }
                ?.sortedByDescending { it.lastModified() }
            if (crashFiles != null && crashFiles.isNotEmpty()) {
                sb.append("===== 最近崩溃报告: ${crashFiles.first().name} =====\n")
                sb.append(crashFiles.first().readText())
                sb.append("\n\n")
            } else {
                sb.append("（无崩溃记录）\n\n")
            }
            // app.log 尾部
            val appLog = File(dir, "app.log")
            if (appLog.exists()) {
                sb.append("===== app.log 尾部 (${appLog.length()} bytes) =====\n")
                sb.append(appLog.readText().takeLast(300_000))
            }
            sb.toString()
        } catch (e: Exception) {
            "导出失败: $e"
        }
    }

    /** 崩溃日志文件列表（Dart 侧用于显示） */
    fun listCrashFiles(): List<String> {
        val ctx = appContext ?: return emptyList()
        return try {
            getLogDir(ctx)?.listFiles { f -> f.name.startsWith("crash_") }
                ?.sortedByDescending { it.lastModified() }
                ?.map { "${it.name} (${it.length()} bytes, ${logDateFormat.format(Date(it.lastModified()))})" }
                ?: emptyList()
        } catch (_: Exception) {
            emptyList()
        }
    }

    /** 日志文件绝对路径（提示用户位置） */
    fun logDirPath(): String {
        val ctx = appContext ?: return "not initialized"
        return getLogDir(ctx)?.absolutePath ?: "no dir"
    }

    private fun getLogDir(ctx: Context): File? {
        return try {
            val dir = File(ctx.filesDir, LOG_DIR)
            if (!dir.exists()) dir.mkdirs()
            dir
        } catch (_: Exception) {
            null
        }
    }
}
