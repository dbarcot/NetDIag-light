//go:build windows

package sysapi

import (
	"context"
	"fmt"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// WinConsole is the Windows Console implementation on the process's
// standard handles.
type WinConsole struct {
	in, out windows.Handle
}

// NewConsole returns the Windows console.
func NewConsole() *WinConsole {
	in, _ := windows.GetStdHandle(windows.STD_INPUT_HANDLE)
	out, _ := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE)
	return &WinConsole{in: in, out: out}
}

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

func getMode(h windows.Handle) (uint32, error) {
	var m uint32
	if err := windows.GetConsoleMode(h, &m); err != nil {
		return 0, fmt.Errorf("%w: %w", ErrNotConsole, err)
	}
	return m, nil
}

func (c *WinConsole) InMode() (uint32, error)  { return getMode(c.in) }
func (c *WinConsole) OutMode() (uint32, error) { return getMode(c.out) }

func (c *WinConsole) SetInMode(mode uint32) error  { return windows.SetConsoleMode(c.in, mode) }
func (c *WinConsole) SetOutMode(mode uint32) error { return windows.SetConsoleMode(c.out, mode) }

func (c *WinConsole) window() (windows.SmallRect, error) {
	var info windows.ConsoleScreenBufferInfo
	if err := windows.GetConsoleScreenBufferInfo(c.out, &info); err != nil {
		return windows.SmallRect{}, fmt.Errorf("%w: %w", ErrNotConsole, err)
	}
	return info.Window, nil
}

func (c *WinConsole) Width() (int, error) {
	w, err := c.window()
	return int(w.Right-w.Left) + 1, err
}

func (c *WinConsole) Height() (int, error) {
	w, err := c.window()
	return int(w.Bottom-w.Top) + 1, err
}

// inputRecord is INPUT_RECORD with the KEY_EVENT_RECORD union member
// flattened, 20 bytes.
type inputRecord struct {
	EventType       uint16
	_               uint16
	KeyDown         int32
	RepeatCount     uint16
	VirtualKeyCode  uint16
	VirtualScanCode uint16
	UnicodeChar     uint16
	ControlKeyState uint32
}

const keyEvent = 0x0001

// keyPollMs bounds how long ReadKey takes to notice a cancelled context.
const keyPollMs = 100

func (c *WinConsole) ReadKey(ctx context.Context) (Key, error) {
	for {
		if err := ctx.Err(); err != nil {
			return Key{}, err
		}
		ev, err := windows.WaitForSingleObject(c.in, keyPollMs)
		if err != nil {
			return Key{}, fmt.Errorf("WaitForSingleObject: %w", err)
		}
		if ev != windows.WAIT_OBJECT_0 {
			continue
		}
		var rec inputRecord
		var n uint32
		r, _, err := procReadConsoleInputW.Call(uintptr(c.in), uintptr(unsafe.Pointer(&rec)), 1, uintptr(unsafe.Pointer(&n)))
		if r == 0 {
			return Key{}, fmt.Errorf("ReadConsoleInputW: %w", err)
		}
		if n == 1 && rec.EventType == keyEvent && rec.KeyDown != 0 {
			return Key{Rune: rune(rec.UnicodeChar), VirtualKey: rec.VirtualKeyCode, Control: rec.ControlKeyState}, nil
		}
	}
}

func (c *WinConsole) Write(p []byte) (int, error) {
	if _, err := getMode(c.out); err != nil {
		return 0, err
	}
	for _, chunk := range splitChunks(utf16.Encode([]rune(string(p))), WriteChunk) {
		for len(chunk) > 0 {
			var written uint32
			if err := windows.WriteConsole(c.out, &chunk[0], uint32(len(chunk)), &written, nil); err != nil {
				return 0, fmt.Errorf("WriteConsoleW: %w", err)
			}
			if written == 0 {
				return 0, fmt.Errorf("WriteConsoleW: nothing written")
			}
			chunk = chunk[written:]
		}
	}
	return len(p), nil
}
