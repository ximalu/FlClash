package com.flclashtier.service.modules

import android.content.Context
import android.net.ConnectivityManager
import android.net.Network
import android.net.NetworkCapabilities
import android.net.NetworkRequest
import android.os.Build
import android.os.ParcelFileDescriptor
import androidx.core.content.getSystemService
import com.flclashtier.common.GlobalState
import java.util.concurrent.ConcurrentHashMap

/**
 * 跟踪「承载 VPN 的真实物理网络」，并把一个 fd 显式绑到它上面。
 *
 * 为什么需要这一层（2026-11-19 真机回归）：
 *
 * `VpnService.protect(fd)` 只做一件事 —— 让这个 socket **绕开我们自己的
 * TUN**。它**不指定**底层用哪张物理网络。而 Android 的 socket 在创建时
 * 就记住了当时的默认网络：WiFi→蜂窝切换后，旧的 ZT UDP socket 仍然挂在
 * 已经失效的 WiFi 上，`sendto()` 返回成功但包被内核直接丢弃 —— 实机表现
 * 正是「wire tx 继续涨、wire rx 归零、业务帧冻结 4 分钟」。
 *
 * 所以新 socket 除了 protect 还必须 `Network.bindSocket(fd)` 到当前物理
 * 网络；只有这样才能保证「重建 socket」真的等于「换了一条活的出网路径」。
 *
 * 选网规则（2026-11-26 复核 + 0.9.22 修正，issues-backlog #12/G1、#15）：
 *
 * 1. **正在 losing 的网络出局**，且这个标记是**粘性**的：`onLosing(maxMsToLive)`
 *    的 `maxMsToLive` 实测常为 0，按「now + maxMsToLive」算窗口就等于没有降权
 *    （0.9.20 的 bug）。现在改为「losing 起 60 s 内视为 losing，或直到该网络重新
 *    报告 VALIDATED / 被移除」。旧实现只按 transport 排序（wifi=0 < cellular=4），
 *    WiFi 关掉后直到 `onLost` 都排第一 —— 换网后前几十秒每次 rebind 都绑到正在死的
 *    WiFi 上，这正是首次换网 88 s 的机制。
 * 2. 候选里**优先已验证（VALIDATED）**的网络，没有才退回未验证的。
 * 3. 同级按传输类型：wifi > ethernet > usb > bluetooth > cellular > satellite。
 *
 * 绑定失败（G2）：Android 没有 unbindSocket，绑错了只能换 socket；这里对同一次
 * 绑定做短重试（4 × 250 ms，通常能赶在系统切换默认网络的窗口内恢复），仍失败才
 * 退回「已 protect 的默认路由」，并计入 `bindFailures` + 写一条 diag 快照。
 *
 * 诊断（#15 观测缺口）：`DiagApp.dump("zt-net"/"zt-bind-fail")` 会把这个对象的状态
 * 写进导出的 `app-diag.log` —— Kotlin 的 `GlobalState.log` 不进用户导出的日志，
 * 所以关键状态必须走 DiagApp 这条路，否则真机日志里永远看不到「绑到哪张网」。
 */
internal class PhysicalNetworkTracker(context: Context) {

    private data class Candidate(
        val capabilities: NetworkCapabilities?,
        val losingSinceMillis: Long = 0,
    ) {
        fun losing(now: Long): Boolean =
            losingSinceMillis > 0 && (now - losingSinceMillis) < LOSING_STICKY_MS

        val validated: Boolean
            get() = capabilities?.hasCapability(NetworkCapabilities.NET_CAPABILITY_VALIDATED) == true
    }

    private val appContext: Context = context.applicationContext

    private val connectivity: ConnectivityManager? = context.getSystemService()

    private val candidates = ConcurrentHashMap<Network, Candidate>()

    @Volatile
    private var best: Network? = null

    @Volatile
    private var bindFailures = 0

    @Volatile
    private var lastBindError: String? = null

    private val request = NetworkRequest.Builder().apply {
        addCapability(NetworkCapabilities.NET_CAPABILITY_NOT_VPN)
        addCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
        addCapability(NetworkCapabilities.NET_CAPABILITY_NOT_RESTRICTED)
    }.build()

    private val callback = object : ConnectivityManager.NetworkCallback() {
        override fun onAvailable(network: Network) {
            update(network, null, validated = false)
        }

        override fun onCapabilitiesChanged(
            network: Network,
            networkCapabilities: NetworkCapabilities,
        ) {
            // 重新拿到 VALIDATED = 这张网又健康了，losing 标记可以清掉。
            update(network, networkCapabilities, validated = true)
        }

        override fun onLosing(network: Network, maxMsToLive: Int) {
            // 换网的关键信号：WiFi 在 onLost 之前会先报 losing，这段时间它已经发不出包了。
            // maxMsToLive 实测常为 0，不能拿它当窗口长度（0.9.20 的 bug）。
            val previous = candidates[network]
            candidates[network] = Candidate(
                previous?.capabilities,
                System.currentTimeMillis().coerceAtLeast(1),
            )
            recompute()
        }

        override fun onLost(network: Network) {
            candidates.remove(network)
            recompute()
        }
    }

    init {
        DiagApp.ztStateProvider = { ztStateJson() }
        runCatching { connectivity?.registerNetworkCallback(request, callback) }
        // 注册前已经存在的网络也要立刻纳入候选（回调只报后续变化）。
        runCatching {
            connectivity?.let { manager ->
                for (network in manager.allNetworks) {
                    val capabilities = manager.getNetworkCapabilities(network) ?: continue
                    if (!capabilities.hasCapability(NetworkCapabilities.NET_CAPABILITY_NOT_VPN)) continue
                    if (!capabilities.hasCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)) continue
                    candidates[network] = Candidate(capabilities)
                }
            }
        }
        recompute()
    }

    @Synchronized
    private fun update(network: Network, capabilities: NetworkCapabilities?, validated: Boolean) {
        val previous = candidates[network]
        val caps = capabilities ?: previous?.capabilities
        val losingSince = if (validated || caps?.hasCapability(
                NetworkCapabilities.NET_CAPABILITY_VALIDATED,
            ) == true
        ) {
            0L
        } else {
            previous?.losingSinceMillis ?: 0L
        }
        candidates[network] = Candidate(caps, losingSince)
        recompute()
    }

    @Synchronized
    private fun recompute() {
        val now = System.currentTimeMillis()
        val known = candidates.entries.filter { (_, value) -> value.capabilities != null }
        // 1) losing 的网络出局（还有别的候选时）。
        val notLosing = known.filter { (_, value) -> !value.losing(now) }
        val pool = if (notLosing.isNotEmpty()) notLosing else known
        // 2) 有已验证的就只在已验证里选。
        val validated = pool.filter { (_, value) -> value.validated }
        val chooseFrom = if (validated.isNotEmpty()) validated else pool
        val next = chooseFrom
            .minByOrNull { (_, value) -> priority(value.capabilities!!) }
            ?.key
        if (next != best) {
            val previous = best?.let { describeNetwork(it) } ?: "none"
            best = next
            GlobalState.log("[ZT-NET] physical network $previous -> ${describe()}")
            // Kotlin 日志不进导出日志，这里额外落一条 diag 快照（#15 观测缺口）。
            DiagApp.dump(appContext, "zt-net")
        }
    }

    private fun priority(capabilities: NetworkCapabilities): Int = when {
        capabilities.hasTransport(NetworkCapabilities.TRANSPORT_WIFI) -> 0
        capabilities.hasTransport(NetworkCapabilities.TRANSPORT_ETHERNET) -> 1
        Build.VERSION.SDK_INT >= Build.VERSION_CODES.S &&
            capabilities.hasTransport(NetworkCapabilities.TRANSPORT_USB) -> 2

        capabilities.hasTransport(NetworkCapabilities.TRANSPORT_BLUETOOTH) -> 3
        capabilities.hasTransport(NetworkCapabilities.TRANSPORT_CELLULAR) -> 4
        Build.VERSION.SDK_INT >= Build.VERSION_CODES.VANILLA_ICE_CREAM &&
            capabilities.hasTransport(NetworkCapabilities.TRANSPORT_SATELLITE) -> 5

        else -> 20
    }

    private fun describeNetwork(network: Network): String {
        val candidate = candidates[network] ?: return "other"
        val capabilities = candidate.capabilities ?: return "unknown"
        val kind = transportName(capabilities)
        val flags = buildList {
            if (candidate.validated) add("validated")
            if (candidate.losing(System.currentTimeMillis())) add("losing")
        }
        return if (flags.isEmpty()) kind else "$kind(${flags.joinToString(",")})"
    }

    private fun transportName(capabilities: NetworkCapabilities): String = when {
        capabilities.hasTransport(NetworkCapabilities.TRANSPORT_WIFI) -> "wifi"
        capabilities.hasTransport(NetworkCapabilities.TRANSPORT_ETHERNET) -> "ethernet"
        Build.VERSION.SDK_INT >= Build.VERSION_CODES.S &&
            capabilities.hasTransport(NetworkCapabilities.TRANSPORT_USB) -> "usb"

        capabilities.hasTransport(NetworkCapabilities.TRANSPORT_BLUETOOTH) -> "bluetooth"
        capabilities.hasTransport(NetworkCapabilities.TRANSPORT_CELLULAR) -> "cellular"
        Build.VERSION.SDK_INT >= Build.VERSION_CODES.VANILLA_ICE_CREAM &&
            capabilities.hasTransport(NetworkCapabilities.TRANSPORT_SATELLITE) -> "satellite"

        else -> "other"
    }

    /** 供日志使用的当前承载网络描述，如 `wifi(validated)` / `none`。 */
    fun describe(): String {
        val network = best ?: return "none"
        return describeNetwork(network)
    }

    /** 绑定诊断：选中的网络 + 累计失败次数 + 最近一条失败原因。 */
    fun statusLine(): String = "${describe()} bindFailures=$bindFailures" +
        (lastBindError?.let { " lastErr=$it" } ?: "")

    /** DiagApp 快照里的 `zt` 段（写进导出的 app-diag.log）。 */
    private fun ztStateJson(): String {
        val sb = StringBuilder(96)
        sb.append('{')
        sb.append("\"net\":\"").append(describe()).append('"')
        sb.append(",\"bindFailures\":").append(bindFailures)
        sb.append(",\"lastBindErr\":").append(if (lastBindError == null) "null" else "\"$lastBindError\"")
        sb.append('}')
        return sb.toString()
    }

    /**
     * 把 fd 绑到当前承载网络。
     *
     * @return null = 成功；非 null = 失败原因（调用方只记日志，不把它当致命错：
     *         绑不上就退回「已 protect 的默认路由」—— 这与 Lunavane 的
     *         fallback 一致，protect 成功已经保证不会打环）。
     *
     * 实现细节：adoptFd(fd) 只是把裸 fd 包成 ParcelFileDescriptor（不复制、
     * 不接管生命周期以外的东西），bindSocket 之后必须 detachFd() 把所有权
     * 还给 Go —— 否则 PFD 被 GC 时会顺手 close 掉 Go 正在用的 fd。
     * bindSocket 的效果作用在 **socket 本身**，与哪个 fd 对象去调无关。
     *
     * 重试：换网瞬间 `bindSocket` 会撞上系统正在搬迁路由的窗口（EINVAL / ENETUNREACH）。
     * 一次失败就放过要等下一个 watchdog 周期（20–60 s），所以这里在**同一次**绑定里
     * 短重试 4 次 × 250 ms；每次重新取 `best`，因为这段时间里 NetworkCallback 可能
     * 刚好切换了候选。
     */
    fun bindSocket(fd: Int): String? {
        var error: String? = null
        for (attempt in 1..BIND_ATTEMPTS) {
            val network = best ?: return "no physical network"
            val descriptor = try {
                ParcelFileDescriptor.adoptFd(fd)
            } catch (error: Exception) {
                return "adoptFd: ${error.javaClass.simpleName}: ${error.message}"
            }
            error = try {
                network.bindSocket(descriptor.fileDescriptor)
                if (attempt > 1) {
                    GlobalState.log("[ZT-SOCKET] bindSocket succeeded on attempt $attempt (net=${describe()})")
                }
                lastBindError = null
                return null
            } catch (error: Exception) {
                "${error.javaClass.simpleName}: ${error.message}"
            } finally {
                runCatching { descriptor.detachFd() }
            }
            if (attempt < BIND_ATTEMPTS) {
                runCatching { Thread.sleep(BIND_RETRY_DELAY_MS) }
            }
        }
        bindFailures++
        lastBindError = error
        // 关键诊断：绑不上时到底选中的是哪张网、失败原因是什么，必须进导出日志。
        DiagApp.dump(appContext, "zt-bind-fail")
        return error
    }

    fun stop() {
        runCatching { connectivity?.unregisterNetworkCallback(callback) }
        candidates.clear()
        best = null
        if (DiagApp.ztStateProvider != null) {
            DiagApp.ztStateProvider = null
        }
    }

    private companion object {
        const val BIND_ATTEMPTS = 4
        const val BIND_RETRY_DELAY_MS = 250L
        /** losing 的粘性窗口：maxMsToLive 常为 0，不能拿它当窗口（0.9.20 的 bug）。 */
        const val LOSING_STICKY_MS = 60_000L
    }
}
