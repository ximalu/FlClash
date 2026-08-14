// ICallbackInterface.aidl
package com.flclashtier.service;

import com.flclashtier.service.IAckInterface;

interface ICallbackInterface {
    oneway void onResult(in byte[] data,in boolean isSuccess, in IAckInterface ack);
}