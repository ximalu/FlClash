package com.flclashtier

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import com.flclashtier.common.BroadcastAction
import com.flclashtier.common.GlobalState
import com.flclashtier.common.action
import kotlinx.coroutines.launch

class BroadcastReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context?, intent: Intent?) {
        when (intent?.action) {
            BroadcastAction.SERVICE_CREATED.action -> {
                GlobalState.log("Receiver service created")
                GlobalState.launch {
                    State.handleStartServiceAction()
                }
            }

            BroadcastAction.SERVICE_DESTROYED.action -> {
                GlobalState.log("Receiver service destroyed")
                GlobalState.launch {
                    State.handleStopServiceAction()
                }
            }
        }
    }
}