package com.flclashtier

import android.app.Application
import android.content.Context
import com.flclashtier.common.GlobalState

class FlClashApplication : Application() {
    override fun attachBaseContext(base: Context?) {
        super.attachBaseContext(base)
        GlobalState.init(this)
    }
}
