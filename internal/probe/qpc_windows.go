//go:build windows

package probe

import (
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Go's monotonic clock on Windows reads the interrupt time, which only
// advances on timer interrupts (0.5–15.6 ms), so sub-millisecond RTTs
// would read as 0. RTTs are timed with QueryPerformanceCounter instead.
var (
	modkernel32                   = windows.NewLazySystemDLL("kernel32.dll")
	procQueryPerformanceCounter   = modkernel32.NewProc("QueryPerformanceCounter")
	procQueryPerformanceFrequency = modkernel32.NewProc("QueryPerformanceFrequency")

	qpcFreqOnce sync.Once
	qpcFreq     int64
)

func qpcNow() int64 {
	var v int64
	_, _, _ = procQueryPerformanceCounter.Call(uintptr(unsafe.Pointer(&v)))
	return v
}

func qpcFrequency() int64 {
	qpcFreqOnce.Do(func() {
		_, _, _ = procQueryPerformanceFrequency.Call(uintptr(unsafe.Pointer(&qpcFreq)))
	})
	return qpcFreq
}

// qpcSince returns the time since a qpcNow reading.
func qpcSince(start int64) time.Duration {
	return ticksToDuration(qpcNow()-start, qpcFrequency())
}

// ticksToDuration converts counter ticks without overflow for any
// realistic probe duration.
func ticksToDuration(ticks, freq int64) time.Duration {
	if freq <= 0 {
		return 0
	}
	sec := ticks / freq
	rem := ticks % freq
	return time.Duration(sec)*time.Second + time.Duration(rem*int64(time.Second)/freq)
}
