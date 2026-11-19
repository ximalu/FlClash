package com.flclashtier.service.modules

import android.content.Context
import android.net.ConnectivityManager
import android.net.Network
import android.net.NetworkCapabilities
import android.net.NetworkRequest
import android.os.Build
import android.os.ParcelFileDescriptor
import androidx.core.content.getSystemService
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
 * 判定物理网络：注册 NOT_VPN + INTERNET 的 NetworkCallback，按传输类型
 * 优先级挑一个（wifi > ethernet > usb > bluetooth > cellular > satellite，
 * 与 NetworkObserveModule 的 DNS 优先级一致），已验证（VALIDATED）的优先。
 */
internal class PhysicalNetworkTracker(context: Context) {

    private data class Candidate(
        val capabilities: NetworkCapabilities?,
    )

    private val connectivity: ConnectivityManager? = context.getSystemService()

    private val candidates = ConcurrentHashMap<Network, Candidate>()

    @Volatile
    private var best: Network? = null

    private val request = NetworkRequest.Builder().apply {
        addCapability(NetworkCapabilities.NET_CAPABILITY_NOT_VPN)
        addCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
        addCapability(NetworkCapabilities.NET_CAPABILITY_NOT_RESTRICTED)
    }.build()

    private val callback = object : ConnectivityManager.NetworkCallback() {
        override fun onAvailable(network: Network) {
            update(network, null)
        }

        override fun onCapabilitiesChanged(
            network: Network,
            networkCapabilities: NetworkCapabilities,
        ) {
            update(network, networkCapabilities)
        }

        override fun onLost(network: Network) {
            candidates.remove(network)
            recompute()
        }
    }

    init {
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
    private fun update(network: Network, capabilities: NetworkCapabilities?) {
        val previous = candidates[network]
        candidates[network] = Candidate(capabilities ?: previous?.capabilities)
        recompute()
    }

    @Synchronized
    private fun recompute() {
        val next = candidates.entries
            .filter { (_, value) -> value.capabilities != null }
            .minByOrNull { (_, value) -> priority(value.capabilities!!) }
            ?.key
        if (next != best) {
            best = next
        }
    }

    private fun priority(capabilities: NetworkCapabilities): Int {
        val transport = when {
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
        // 已验证（真的能出网）的网络优先于同级未验证的。
        val validated = if (capabilities.hasCapability(NetworkCapabilities.NET_CAPABILITY_VALIDATED)) 0 else 10
        return transport + validated
    }

    /** 供日志使用的当前承载网络描述，如 `wifi(validated)` / `none`。 */
    fun describe(): String {
        val network = best ?: return "none"
        val capabilities = candidates[network]?.capabilities
        val kind = when {
            capabilities == null -> "unknown"
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
        val validated = capabilities?.hasCapability(NetworkCapabilities.NET_CAPABILITY_VALIDATED) == true
        return if (validated) "$kind(validated)" else kind
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
     */
    fun bindSocket(fd: Int): String? {
        val network = best ?: return "no physical network"
        val descriptor = try {
            ParcelFileDescriptor.adoptFd(fd)
        } catch (error: Exception) {
            return "adoptFd: ${error.javaClass.simpleName}: ${error.message}"
        }
        return try {
            network.bindSocket(descriptor.fileDescriptor)
            null
        } catch (error: Exception) {
            "${error.javaClass.simpleName}: ${error.message}"
        } finally {
            runCatching { descriptor.detachFd() }
        }
    }

    fun stop() {
        runCatching { connectivity?.unregisterNetworkCallback(callback) }
        candidates.clear()
        best = null
    }
}
