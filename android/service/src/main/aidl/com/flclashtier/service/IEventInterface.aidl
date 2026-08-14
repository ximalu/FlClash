// IEventInterface.aidl
package com.flclashtier.service;

import com.flclashtier.service.IAckInterface;

interface IEventInterface {
    oneway void onEvent(in String id, in byte[] data,in boolean isSuccess, in IAckInterface ack);
}