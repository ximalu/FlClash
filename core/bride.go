//go:build android && cgo

package main

//#include "bride.h"
import "C"
import "unsafe"

// protect 返回该 fd 是否真的被排除在 VPN 之外（并绑到当前物理网络）。
//
// 返回值必须一路上到 engine：protect 失败 = socket 仍会被自己的 TUN 捕获
// → ZT UDP 在 TUN→ZT 之间打转。engine 侧对这种 fd 是 fail closed（宁可
// 启动失败），所以这里绝不能把失败吞成 void。
func protect(callback unsafe.Pointer, fd int) bool {
	if callback == nil {
		return false
	}
	return C.protect(callback, C.int(fd)) != 0
}

func resolveProcess(callback unsafe.Pointer, protocol int, source, target string, uid int) string {
	s := C.CString(source)
	defer C.free(unsafe.Pointer(s))
	t := C.CString(target)
	defer C.free(unsafe.Pointer(t))
	res := C.resolve_process(callback, C.int(protocol), s, t, C.int(uid))
	return takeCString(res)
}

func invokeResult(callback unsafe.Pointer, data string) {
	s := C.CString(data)
	defer C.free(unsafe.Pointer(s))
	C.result(callback, s)
}

func releaseObject(callback unsafe.Pointer) {
	C.release_object(callback)
}

func takeCString(s *C.char) string {
	defer C.free_string(s)
	return C.GoString(s)
}
