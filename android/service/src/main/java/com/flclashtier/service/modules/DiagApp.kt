package com.flclashtier.service.modules

import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.net.ConnectivityManager
import android.net.LinkProperties
import android.net.Network
import android.net.NetworkCapabilities
import android.os.BatteryManager
import android.os.Build
import android.os.Handler
import android.os.Looper
import android.os.PowerManager
import android.os.SystemClock
import androidx.core.content.getSystemService
import java.io.File
import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale

/**
 * 应用/系统侧诊断快照（2026-11-24 用户要求：日志要能回答「当时手机是什么状态」）。
 *
 * 数据面故障（ZT 帧不动）经常与手机侧状态强相关：换网、熄屏 / Doze、省电模式、
 * VPN 重建、DNS 变化、captive portal……这些在 Go 侧看不到，只有
 * Context / ConnectivityManager / PowerManager 才知道，所以由这里负责，
 * 每隔一段时间（以及每次网络回调）往 `<files>/logs/app-diag.log` 追加一行 JSON，
 * 导出日志时一起带上。
 *
 * 约束：诊断代码绝不允许影响主流程 —— 全部异常吞掉，写入失败静默放弃。
 */
internal object DiagApp {
    private const val MAX_BYTES = 2 * 1024 * 1024
    private const val PERIOD_MS = 60_000L

    private val lock = Any()
    private val tsFormat = SimpleDateFormat("yyyy-MM-dd'T'HH:mm:ss.SSS", Locale.US)
    private var handler: Handler? = null
    private var tick: Runnable? = null

    fun dump(context: Context, tag: String) {
        try {
            val app = context.applicationContext
            val line = build(app, tag)
            synchronized(lock) { append(app, line) }
        } catch (_: Throwable) {
            // 诊断不能影响主流程
        }
    }

    /** 周期性快照（VPN 服务运行期间由 NetworkObserveModule 启停）。 */
    fun startPeriodic(context: Context) {
        stopPeriodic()
        try {
            val app = context.applicationContext
            val h = Handler(Looper.getMainLooper())
            val r = object : Runnable {
                override fun run() {
                    dump(app, "periodic")
                    h.postDelayed(this, PERIOD_MS)
                }
            }
            handler = h
            tick = r
            h.postDelayed(r, PERIOD_MS)
        } catch (_: Throwable) {
        }
    }

    fun stopPeriodic() {
        try {
            handler?.let { h -> tick?.let { h.removeCallbacks(it) } }
        } catch (_: Throwable) {
        } finally {
            handler = null
            tick = null
        }
    }

    private fun append(app: Context, line: String) {
        val dir = File(app.filesDir, "logs")
        if (!dir.exists()) dir.mkdirs()
        val file = File(dir, "app-diag.log")
        if (file.exists() && file.length() > MAX_BYTES) {
            val old = File(dir, "app-diag.log.1")
            if (old.exists()) old.delete()
            file.renameTo(old)
        }
        file.appendText(line + "\n")
    }

    private fun build(app: Context, tag: String): String {
        val cm = app.getSystemService<ConnectivityManager>()
        val pm = app.getSystemService<PowerManager>()
        val sb = StringBuilder(512)
        sb.append("{\"t\":\"").append(tsFormat.format(Date())).append('"')
        sb.append(",\"tag\":\"").append(tag).append('"')
        sb.append(",\"sdk\":").append(Build.VERSION.SDK_INT)
        sb.append(",\"uptimeMs\":").append(SystemClock.elapsedRealtime())
        sb.append(",\"screenOn\":").append(if (pm?.isInteractive == true) "true" else "false")
        sb.append(",\"powerSave\":").append(if (pm?.isPowerSaveMode == true) "true" else "false")
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.M) {
            sb.append(",\"deviceIdle\":").append(if (pm?.isDeviceIdleMode == true) "true" else "false")
        }
        sb.append(",\"battery\":").append(battery(app))
        sb.append(",\"charging\":").append(if (charging(app)) "true" else "false")

        val active = cm?.activeNetwork
        sb.append(",\"active\":").append(describeNetwork(cm, active))
        sb.append(",\"nets\":[")
        var first = true
        try {
            cm?.allNetworks?.forEach { n ->
                if (!first) sb.append(',')
                first = false
                sb.append(describeNetwork(cm, n))
            }
        } catch (_: Throwable) {
        }
        sb.append("]}")
        return sb.toString()
    }

    private fun describeNetwork(cm: ConnectivityManager?, network: Network?): String {
        if (cm == null || network == null) return "null"
        val sb = StringBuilder(160)
        sb.append('{')
        val caps = try {
            cm.getNetworkCapabilities(network)
        } catch (_: Throwable) {
            null
        }
        val lp = try {
            cm.getLinkProperties(network)
        } catch (_: Throwable) {
            null
        }
        sb.append("\"transports\":[").append(transports(caps)).append(']')
        sb.append(",\"validated\":").append(if (caps?.hasCapability(NetworkCapabilities.NET_CAPABILITY_VALIDATED) == true) "true" else "false")
        sb.append(",\"internet\":").append(if (caps?.hasCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET) == true) "true" else "false")
        sb.append(",\"notMetered\":").append(if (caps?.hasCapability(NetworkCapabilities.NET_CAPABILITY_NOT_METERED) == true) "true" else "false")
        if (caps != null) {
            sb.append(",\"downKbps\":").append(caps.linkDownstreamBandwidthKbps)
            sb.append(",\"upKbps\":").append(caps.linkUpstreamBandwidthKbps)
        }
        sb.append(",\"iface\":").append('"').append(lp?.interfaceName ?: "").append('"')
        sb.append(",\"mtu\":").append(ifaceMtu(lp))
        sb.append(",\"dns\":[").append(dns(lp)).append(']')
        sb.append('}')
        return sb.toString()
    }

    private fun transports(caps: NetworkCapabilities?): String {
        if (caps == null) return ""
        val out = mutableListOf<String>()
        if (caps.hasTransport(NetworkCapabilities.TRANSPORT_WIFI)) out += "\"wifi\""
        if (caps.hasTransport(NetworkCapabilities.TRANSPORT_CELLULAR)) out += "\"cellular\""
        if (caps.hasTransport(NetworkCapabilities.TRANSPORT_ETHERNET)) out += "\"ethernet\""
        if (caps.hasTransport(NetworkCapabilities.TRANSPORT_BLUETOOTH)) out += "\"bluetooth\""
        if (caps.hasTransport(NetworkCapabilities.TRANSPORT_VPN)) out += "\"vpn\""
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S &&
            caps.hasTransport(NetworkCapabilities.TRANSPORT_USB)
        ) {
            out += "\"usb\""
        }
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.VANILLA_ICE_CREAM &&
            caps.hasTransport(NetworkCapabilities.TRANSPORT_SATELLITE)
        ) {
            out += "\"satellite\""
        }
        return out.joinToString(",")
    }

    private fun ifaceMtu(lp: LinkProperties?): Int = try {
        lp?.mtu ?: 0
    } catch (_: Throwable) {
        0
    }

    private fun dns(lp: LinkProperties?): String = try {
        lp?.dnsServers?.joinToString(",") { "\"${it.hostAddress ?: ""}\"" } ?: ""
    } catch (_: Throwable) {
        ""
    }

    private fun battery(context: Context): Int {
        return try {
            val bm = context.getSystemService<BatteryManager>()
            bm?.getIntProperty(BatteryManager.BATTERY_PROPERTY_CAPACITY) ?: -1
        } catch (_: Throwable) {
            -1
        }
    }

    private fun charging(context: Context): Boolean {
        return try {
            val intent = context.registerReceiver(null, IntentFilter(Intent.ACTION_BATTERY_CHANGED))
            val status = intent?.getIntExtra(BatteryManager.EXTRA_STATUS, -1) ?: -1
            status == BatteryManager.BATTERY_STATUS_CHARGING || status == BatteryManager.BATTERY_STATUS_FULL
        } catch (_: Throwable) {
            false
        }
    }
}
