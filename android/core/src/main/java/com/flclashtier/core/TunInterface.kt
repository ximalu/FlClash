package com.flclashtier.core

import androidx.annotation.Keep

@Keep
interface TunInterface {
    /**
     * 把 fd 排除在 VPN 之外，并（在 Android 上）把它绑到当前承载物理网络。
     *
     * 返回值必须真实反映结果：false = 这个 socket 仍会被我们自己的 TUN 捕获，
     * ZT UDP 会在 TUN→ZT 之间打转。Go 侧对 false 是 fail closed（放弃这个
     * socket），所以实现里绝不允许静默吞掉失败。
     */
    fun protect(fd: Int): Boolean

    fun resolverProcess(protocol: Int, source: String, target: String, uid: Int): String
}
