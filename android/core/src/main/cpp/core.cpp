#include <jni.h>

#ifdef LIBCLASH

#include "jni_helper.h"
#include "libclash.h"
#include "bride.h"

extern "C"
JNIEXPORT void JNICALL
Java_com_flclashtier_core_Core_startTun(JNIEnv *env, jobject thiz, jint fd, jobject cb,
                                         jstring stack, jstring address, jstring dns) {
    const auto interface = new_global(cb);
    startTUN(interface, fd, get_string(stack), get_string(address), get_string(dns));
}

extern "C"
JNIEXPORT void JNICALL
Java_com_flclashtier_core_Core_stopTun(JNIEnv *env, jobject thiz) {
    stopTun();
}

extern "C"
JNIEXPORT void JNICALL
Java_com_flclashtier_core_Core_forceGC(JNIEnv *env, jobject thiz) {
    forceGC();
}

extern "C"
JNIEXPORT void JNICALL
Java_com_flclashtier_core_Core_updateDNS(JNIEnv *env, jobject thiz, jstring dns) {
    updateDns(get_string(dns));
}

extern "C"
JNIEXPORT void JNICALL
Java_com_flclashtier_core_Core_invokeMethod(JNIEnv *env, jobject thiz, jstring data, jobject cb) {
    const auto interface = new_global(cb);
    invokeMethod(interface, get_string(data));
}

extern "C"
JNIEXPORT void JNICALL
Java_com_flclashtier_core_Core_setEventListener(JNIEnv *env, jobject thiz, jobject cb) {
    if (cb != nullptr) {
        const auto interface = new_global(cb);
        setEventListener(interface);
    } else {
        setEventListener(nullptr);
    }
}

extern "C"
JNIEXPORT jstring JNICALL
Java_com_flclashtier_core_Core_getTraffic(JNIEnv *env, jobject thiz,
                                           const jboolean only_statistics_proxy) {
    scoped_string traffic = getTraffic(only_statistics_proxy);
    return new_string(traffic);
}

extern "C"
JNIEXPORT jstring JNICALL
Java_com_flclashtier_core_Core_getTotalTraffic(JNIEnv *env, jobject thiz,
                                                const jboolean only_statistics_proxy) {
    scoped_string traffic = getTotalTraffic(only_statistics_proxy);
    return new_string(traffic);
}

extern "C"
JNIEXPORT void JNICALL
Java_com_flclashtier_core_Core_suspended(JNIEnv *env, jobject thiz, jboolean suspended) {
    suspend(suspended);
}

extern "C"
JNIEXPORT void JNICALL
Java_com_flclashtier_core_Core_quickSetup(JNIEnv *env, jobject thiz, jstring init_params_string,
                                           jstring setup_params_string, jobject cb) {
    const auto interface = new_global(cb);
    quickSetup(interface, get_string(init_params_string), get_string(setup_params_string));
}


static jmethodID m_tun_interface_protect;
static jmethodID m_tun_interface_resolve_process;
static jmethodID m_invoke_interface_result;


static void release_jni_object_impl(void *obj) {
    ATTACH_JNI();
    del_global(static_cast<jobject>(obj));
}

static void free_string_impl(char *str) {
    free(str);
}

// 返回 int（而不是 jboolean）以匹配 bride.h 里的函数指针类型：
// jboolean 是 unsigned char，函数指针类型不兼容会直接编译失败。
static int call_tun_interface_protect_impl(void *tun_interface, const int fd) {
    ATTACH_JNI();
    const jboolean result = env->CallBooleanMethod(static_cast<jobject>(tun_interface),
                                                   m_tun_interface_protect,
                                                   fd);
    // 异常不能留到下一条 JNI 调用：protect 抛异常在这里就地清掉并返回失败，
    // 让 Go 侧走 fail-closed，而不是在别处炸出一个不相干的 JNI 错误。
    if (env->ExceptionCheck()) {
        env->ExceptionDescribe();
        env->ExceptionClear();
        return 0;
    }
    return result == JNI_TRUE ? 1 : 0;
}

static char *
call_tun_interface_resolve_process_impl(void *tun_interface, const int protocol,
                                        const char *source,
                                        const char *target,
                                        const int uid) {
    ATTACH_JNI();
    const auto source_string = new_string(source);
    const auto target_string = new_string(target);
    const auto package_name = reinterpret_cast<jstring>(env->CallObjectMethod(
            static_cast<jobject>(tun_interface),
            m_tun_interface_resolve_process,
            protocol,
            source_string,
            target_string,
            uid));
    env->DeleteLocalRef(source_string);
    env->DeleteLocalRef(target_string);
    const auto result = get_string(package_name);
    if (package_name != nullptr) {
        env->DeleteLocalRef(package_name);
    }
    return result;
}

static void call_invoke_interface_result_impl(void *invoke_interface, const char *data) {
    ATTACH_JNI();
    const auto value = new_string(data);
    env->CallVoidMethod(static_cast<jobject>(invoke_interface),
                        m_invoke_interface_result,
                        value);
    env->DeleteLocalRef(value);
}

extern "C"
JNIEXPORT jint JNICALL
JNI_OnLoad(JavaVM *vm, void *) {
    JNIEnv *env = nullptr;
    if (vm->GetEnv(reinterpret_cast<void **>(&env), JNI_VERSION_1_6) != JNI_OK) {
        return JNI_ERR;
    }

    initialize_jni(vm, env);

    const auto c_tun_interface = find_class("com/flclashtier/core/TunInterface");

    const auto c_invoke_interface = find_class("com/flclashtier/core/InvokeInterface");

    m_tun_interface_protect = find_method(c_tun_interface, "protect", "(I)Z");
    m_tun_interface_resolve_process = find_method(c_tun_interface, "resolverProcess",
                                                  "(ILjava/lang/String;Ljava/lang/String;I)Ljava/lang/String;");
    m_invoke_interface_result = find_method(c_invoke_interface, "onResult",
                                            "(Ljava/lang/String;)V");


    protect_func = &call_tun_interface_protect_impl;
    resolve_process_func = &call_tun_interface_resolve_process_impl;
    result_func = &call_invoke_interface_result_impl;
    release_object_func = &release_jni_object_impl;
    free_string_func = &free_string_impl;

    return JNI_VERSION_1_6;
}
#else
extern "C"
JNIEXPORT void JNICALL
Java_com_flclashtier_core_Core_startTun(JNIEnv *env, jobject thiz, jint fd, jobject cb,
                                         jstring stack, jstring address, jstring dns) {
}

extern "C"
JNIEXPORT void JNICALL
Java_com_flclashtier_core_Core_stopTun(JNIEnv *env, jobject thiz) {
}

extern "C"
JNIEXPORT void JNICALL
Java_com_flclashtier_core_Core_invokeMethod(JNIEnv *env, jobject thiz, jstring data, jobject cb) {
}

extern "C"
JNIEXPORT void JNICALL
Java_com_flclashtier_core_Core_forceGC(JNIEnv *env, jobject thiz) {
}

extern "C"
JNIEXPORT void JNICALL
Java_com_flclashtier_core_Core_updateDNS(JNIEnv *env, jobject thiz, jstring dns) {
}

extern "C"
JNIEXPORT void JNICALL
Java_com_flclashtier_core_Core_setEventListener(JNIEnv *env, jobject thiz, jobject cb) {
}

extern "C"
JNIEXPORT jstring JNICALL
Java_com_flclashtier_core_Core_getTraffic(JNIEnv *env, jobject thiz,
                                           const jboolean only_statistics_proxy) {
    return env->NewStringUTF("{}");
}
extern "C"
JNIEXPORT jstring JNICALL
Java_com_flclashtier_core_Core_getTotalTraffic(JNIEnv *env, jobject thiz,
                                                const jboolean only_statistics_proxy) {
    return env->NewStringUTF("{}");
}

extern "C"
JNIEXPORT void JNICALL
Java_com_flclashtier_core_Core_suspended(JNIEnv *env, jobject thiz, jboolean suspended) {
}

extern "C"
JNIEXPORT void JNICALL
Java_com_flclashtier_core_Core_quickSetup(JNIEnv *env, jobject thiz, jstring init_params_string,
                                           jstring setup_params_string, jobject cb) {
}
#endif
