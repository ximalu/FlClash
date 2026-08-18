//go:build android && cgo

package zerotier

/*
#cgo LDFLAGS: ${SRCDIR}/wrapper_android.o ${SRCDIR}/lib/libzerotiercore-android.a -static-libstdc++ -llog -lm
#include "wrapper.h"
*/
import "C"
