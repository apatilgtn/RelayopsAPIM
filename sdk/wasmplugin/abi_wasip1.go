//go:build wasip1

package wasmplugin

import (
	"runtime"
	"unsafe"
)

// The gateway ABI: the host calls allocate(n) for an input buffer, writes the
// request JSON there, then calls execute_policy(ptr, n), which returns the
// result JSON as (ptr << 32 | len). Both buffers live in package variables
// until the next call, which is safe because an instance serves one request
// at a time.
var inBuf, outBuf []byte

//go:wasmexport allocate
func allocate(size uint32) uint32 {
	if cap(inBuf) < int(size) || cap(inBuf) == 0 {
		inBuf = make([]byte, max(size, 1))
	}
	inBuf = inBuf[:size]
	return uint32(uintptr(unsafe.Pointer(unsafe.SliceData(inBuf[:1]))))
}

//go:wasmexport execute_policy
func executePolicy(ptr, size uint32) uint64 {
	outBuf = Dispatch(handler, inBuf[:size])
	if len(outBuf) == 0 {
		return 0
	}
	return uint64(uintptr(unsafe.Pointer(unsafe.SliceData(outBuf))))<<32 | uint64(len(outBuf))
}

//go:wasmexport execute_response
func executeResponse(ptr, size uint32) uint64 {
	outBuf = DispatchResponse(responseHandler, inBuf[:size])
	if len(outBuf) == 0 {
		return 0
	}
	return uint64(uintptr(unsafe.Pointer(unsafe.SliceData(outBuf))))<<32 | uint64(len(outBuf))
}

//go:wasmexport relayops_phases
func relayopsPhases() int32 { return Phases() }

//go:wasmimport env relayops_log
func hostLog(level int32, ptr, size uint32)

//go:wasmimport env relayops_now_ms
func hostNowMS() int64

// Log writes to the gateway's log at a level: 1 debug, 2 info, 3 warn, 4 error.
func Log(level int, msg string) {
	if msg == "" {
		return
	}
	b := []byte(msg)
	hostLog(int32(level), uint32(uintptr(unsafe.Pointer(unsafe.SliceData(b)))), uint32(len(b)))
	runtime.KeepAlive(b)
}

// NowMS is the gateway's clock in Unix milliseconds.
func NowMS() int64 { return hostNowMS() }
