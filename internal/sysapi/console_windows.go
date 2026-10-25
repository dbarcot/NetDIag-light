//go:build windows

package sysapi

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var procGetConsoleProcessList = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleProcessList")

// WinConsole is the Windows Console implementation.
type WinConsole struct{}

// NewConsole returns the Windows console.
func NewConsole() *WinConsole { return &WinConsole{} }

func (WinConsole) ProcessCount() (int, error) {
	// The list is only needed for its length; when the buffer is too small
	// the call still returns the total count.
	var ids [4]uint32
	n, _, err := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&ids[0])), uintptr(len(ids)))
	if n == 0 {
		return 0, err
	}
	return int(n), nil
}
