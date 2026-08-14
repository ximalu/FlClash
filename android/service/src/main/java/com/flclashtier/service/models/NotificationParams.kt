package com.flclashtier.service.models

data class NotificationParams(
    val title: String = "FlClash",
    val stopText: String = "STOP",
    val onlyStatisticsProxy: Boolean = false,
)
