//go:build linux && !android && cgo

package zerotier

/*
#cgo LDFLAGS: ${SRCDIR}/wrapper_linux.o ${SRCDIR}/lib/libzerotiercore-linux.a -lstdc++ -lm -lpthread
#include "wrapper.h"
*/
import "C"
