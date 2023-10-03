package validation

/*
#cgo LDFLAGS: ./validation/chess-finder-core.so
#include <stdlib.h>
#include <stdio.h>
#include "chess-finder-core.h"
*/
import "C"

import (
	"fmt"
	"unsafe"
)

func ValidateBoard(board string) bool {
	var isolate *C.graal_isolate_t = nil
	var thread *C.graal_isolatethread_t = nil

	if C.graal_create_isolate(nil, &isolate, &thread) != 0 {
		fmt.Println("Initialization error")
		return false
	}

	defer C.graal_tear_down_isolate(thread)
	cstr := C.CString(board)
	defer C.free(unsafe.Pointer(cstr))
	isValid := C.validate(thread, cstr)

	return isValid != 0
}
