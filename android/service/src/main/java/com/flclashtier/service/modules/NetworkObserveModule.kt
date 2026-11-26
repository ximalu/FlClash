package com.flclashtier.service.modules

import android.app.Service
import android.net.ConnectivityManager
import android.net.LinkProperties
import android.net.Network
import android.net.NetworkCapabilities
import android.net.NetworkCapabilities.TRANSPORT_SATELLITE
import android.net.NetworkCapabilities.TRANSPORT_USB
import android.net.NetworkRequest
import android.os.Build
import androidx.core.content.getSystemService
import com.flclashtier.core.Core
import java.net.Inet4Address
import java.net.Inet6Address
import java.net.InetAddress
import java.util.concurrent.ConcurrentHashMap

private data class NetworkInfo(
    @Volatile var losingUntilMillis: Long = 0,
    @Volatile var dnsList: List<InetAddress> = emptyList(),
    @Volatile var addrs: List<InetAddress> = emptyList(),
) {
    val priorityPenalty: Int
        get() = if (losingUntilMillis > System.currentTimeMillis()) 10 else 0
}

internal class NetworkObserveModule(private val service: Service) : ServiceModule {

    private val networkInfos = ConcurrentHashMap<Network, NetworkInfo>()
    private val connectivity by lazy {
        service.getSystemService<ConnectivityManager>()
    }
    private var currentDnsList = listOf<String>()
    private var currentLocalAddrs = listOf<String>()

    private val request = NetworkRequest.Builder().apply {
        addCapability(NetworkCapabilities.NET_CAPABILITY_NOT_VPN)
        addCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.P) {
            addCapability(NetworkCapabilities.NET_CAPABILITY_FOREGROUND)
        }
        addCapability(NetworkCapabilities.NET_CAPABILITY_NOT_RESTRICTED)
    }.build()

    private val callback = object : ConnectivityManager.NetworkCallback() {
        override fun onAvailable(network: Network) {
            networkInfos[network] = NetworkInfo()
            updateDns()
            DiagApp.dump(service, "net-available")
        }

        override fun onLosing(network: Network, maxMsToLive: Int) {
            networkInfos[network]?.losingUntilMillis = System.currentTimeMillis() + maxMsToLive
            updateDns()
        }

        override fun onLost(network: Network) {
            networkInfos.remove(network)
            updateDns()
            DiagApp.dump(service, "net-lost")
        }

        override fun onLinkPropertiesChanged(network: Network, linkProperties: LinkProperties) {
            val info = networkInfos[network]
            if (info != null) {
                info.dnsList = linkProperties.dnsServers
                info.addrs = linkProperties.linkAddresses.map { it.address }
            }
            updateDns()
            DiagApp.dump(service, "net-linkprops")
        }
    }

    override fun start() {
        updateDns()
        connectivity?.registerNetworkCallback(request, callback)
        DiagApp.dump(service, "service-start")
        DiagApp.startPeriodic(service)
    }

    private fun networkPriority(entry: Map.Entry<Network, NetworkInfo>): Int {
        val capabilities = connectivity?.getNetworkCapabilities(entry.key)
        return when {
            capabilities == null -> 100
            capabilities.hasTransport(NetworkCapabilities.TRANSPORT_VPN) -> 90
            capabilities.hasTransport(NetworkCapabilities.TRANSPORT_WIFI) -> 0
            capabilities.hasTransport(NetworkCapabilities.TRANSPORT_ETHERNET) -> 1
            Build.VERSION.SDK_INT >= Build.VERSION_CODES.S &&
                capabilities.hasTransport(TRANSPORT_USB) -> 2

            capabilities.hasTransport(NetworkCapabilities.TRANSPORT_BLUETOOTH) -> 3
            capabilities.hasTransport(NetworkCapabilities.TRANSPORT_CELLULAR) -> 4
            Build.VERSION.SDK_INT >= Build.VERSION_CODES.VANILLA_ICE_CREAM &&
                capabilities.hasTransport(TRANSPORT_SATELLITE) -> 5

            else -> 20
        } + entry.value.priorityPenalty
    }

    @Synchronized
    private fun updateDns() {
        val dnsList = networkInfos.asSequence()
            .minByOrNull(::networkPriority)
            ?.value
            ?.dnsList
            .orEmpty()
            .map { address -> address.asSocketAddressText(DNS_PORT) }
            .distinct()
        if (dnsList != currentDnsList) {
            currentDnsList = dnsList
            Core.updateDNS(dnsList.joinToString(","))
        }
        // 必须在早退之外：DNS 没变也要把本地地址同步给 core（换网时 DNS 可能不变，
        // 但 v6 地址变了 —— 0.9.30 把它们放在早退之后，结果一条都没报出去）。
        updateLocalAddrs()
    }

    /// 把当前优选网络的本地地址注册给 ZT core（2026-11-27）。
    ///
    /// core 不会自己枚举 Android 网卡：它靠对端 SelfAwareness 反推我们的外部端点，
    /// 而反推要求**已经有包进出**。不告诉它本机有 v6 地址，它就永远只往 v4 走 ——
    /// 而蜂窝的 v4 是 CGNAT（打洞 3 分钟级），v6 本该是秒级直连。
    ///
    /// 只报 global unicast：排除 loopback、link-local（fe80::）与 ULA（fc00::/7）——
    /// fe80:: 给出去只会让 core 去试一条不可能通的路径。
    @Synchronized
    private fun updateLocalAddrs() {
        val addrs = networkInfos.asSequence()
            .minByOrNull(::networkPriority)
            ?.value
            ?.addrs
            .orEmpty()
            .filter { addr ->
                val ula = addr is Inet6Address && (addr.address[0].toInt() and 0xfe) == 0xfc
                !addr.isLoopbackAddress &&
                    !addr.isLinkLocalAddress &&
                    !addr.isSiteLocalAddress &&
                    !ula
            }
            .map { addr -> addr.hostAddress ?: return@map "" }
            .filter { it.isNotEmpty() }
            .distinct()
        if (addrs == currentLocalAddrs) {
            return
        }
        currentLocalAddrs = addrs
        val payload = addrs.joinToString(",")
        // 诊断：这条 dump 里 nets[].addrs/hasIpv6 会直接告诉我们当时每张网络有没有 v6。
        DiagApp.dump(service, "zt-local-addrs")
        Core.invokeMethod(
            "{\"id\":\"zt-local-addrs\",\"method\":\"setZtLocalAddresses\",\"arguments\":\"$payload\"}",
        ) {}
    }

    override fun stop() {
        DiagApp.dump(service, "service-stop")
        DiagApp.stopPeriodic()
        try {
            connectivity?.unregisterNetworkCallback(callback)
        } finally {
            networkInfos.clear()
            updateDns()
        }
    }
}

private const val DNS_PORT = 53

private fun InetAddress.asSocketAddressText(port: Int): String = when (this) {
    is Inet6Address -> "[$hostAddress]:$port"
    is Inet4Address -> "$hostAddress:$port"
    else -> error("Unsupported address type: ${javaClass.name}")
}
