// IRemoteInterface.aidl
package com.flclashtier.service;

import com.flclashtier.service.ICallbackInterface;
import com.flclashtier.service.IEventInterface;
import com.flclashtier.service.IResultInterface;
import com.flclashtier.service.IVoidInterface;
import com.flclashtier.service.models.VpnOptions;
import com.flclashtier.service.models.NotificationParams;

interface IRemoteInterface {
    void invokeAction(in String data, in ICallbackInterface callback);
    void quickSetup(in String initParamsString, in String setupParamsString, in ICallbackInterface callback, in IVoidInterface onStarted);
    void updateNotificationParams(in NotificationParams params);
    void startService(in VpnOptions options, in long runTime, in IResultInterface result);
    void stopService(in IResultInterface result);
    void setEventListener(in IEventInterface event);
    void setCrashlytics(in boolean enable);
    long getRunTime();
}