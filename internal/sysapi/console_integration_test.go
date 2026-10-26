//go:build integration && windows

package sysapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

const freshConsoleEnv = "NETDIAG_FRESH_CONSOLE_RESULT"

type freshConsoleResult struct {
	ProcessCount    int
	OrigIn, OrigOut uint32
	RawIn, RawOut   uint32
	RestIn, RestOut uint32
	VT, Keys        bool
	Written         string // read back from the screen buffer after Write
	BigFrameLast    string // last row of a ~40 kB frame, read back
	Err             string
}

// bigFrame is a ~40 kB frame of coloured rows (several WriteConsoleW
// chunks); its last row reads back as bigFrameLastRow.
func bigFrame() string {
	var b strings.Builder
	for i := range 500 {
		if i > 0 {
			b.WriteString("\r\n")
		}
		fmt.Fprintf(&b, "\x1b[1;32mrow %03d\x1b[0m %s\x1b[K", i, strings.Repeat("▁▂▃▄", 15))
	}
	return b.String()
}

var bigFrameLastRow = "row 499 " + strings.Repeat("▁▂▃▄", 15)

// consoleWriteProbe has characters outside every OEM code page.
const consoleWriteProbe = "spark ▁▄█ × …"

// TestWinConsole_FreshConsole re-runs itself in a new hidden console, as a
// double-click from Explorer would, and checks there: the process owns the
// console alone (count 1), EnterRaw sets the SPEC §9 modes on a real
// console, and Restore returns both original modes.
func TestWinConsole_FreshConsole(t *testing.T) {
	if out := os.Getenv(freshConsoleEnv); out != "" {
		writeFreshConsoleResult(out)
		return
	}
	resultFile := filepath.Join(t.TempDir(), "result.json")
	cmd := exec.Command(os.Args[0], "-test.run=^TestWinConsole_FreshConsole$")
	cmd.Env = append(os.Environ(), freshConsoleEnv+"="+resultFile)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_CONSOLE, HideWindow: true}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
	data, err := os.ReadFile(resultFile)
	if err != nil {
		t.Fatal(err)
	}
	var r freshConsoleResult
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	t.Logf("%+v", r)
	if r.Err != "" {
		t.Fatalf("child error: %s", r.Err)
	}
	if r.ProcessCount != 1 {
		t.Errorf("ProcessCount in own console = %d, want 1 (double-click detection)", r.ProcessCount)
	}
	if !r.VT || !r.Keys {
		t.Errorf("VT=%v Keys=%v on a real console", r.VT, r.Keys)
	}
	if r.RawIn&(ModeLineInput|ModeEchoInput|ModeQuickEdit) != 0 || r.RawIn&ModeProcessedInput == 0 {
		t.Errorf("raw stdin mode %#x", r.RawIn)
	}
	if r.RawOut&ModeVTProcessing == 0 {
		t.Errorf("raw stdout mode %#x lacks VT", r.RawOut)
	}
	if r.RestIn != r.OrigIn || r.RestOut != r.OrigOut {
		t.Errorf("not restored: in %#x→%#x, out %#x→%#x", r.OrigIn, r.RestIn, r.OrigOut, r.RestOut)
	}
	if r.BigFrameLast != bigFrameLastRow {
		t.Errorf("last row of a big frame %q, want %q (chunked writes)", r.BigFrameLast, bigFrameLastRow)
	}
	if r.Written != consoleWriteProbe {
		t.Errorf("Write put %q on the screen, want %q (WriteConsoleW, SPEC §9 encoding)", r.Written, consoleWriteProbe)
	}
}

func writeFreshConsoleResult(path string) {
	var r freshConsoleResult
	defer func() {
		b, _ := json.Marshal(r)
		_ = os.WriteFile(path, b, 0o644)
	}()
	c, closeFn, err := openConsoleDevices()
	if err != nil {
		r.Err = err.Error()
		return
	}
	defer closeFn()
	r.ProcessCount, _ = c.ProcessCount()
	r.OrigIn, _ = c.InMode()
	r.OrigOut, _ = c.OutMode()
	s := EnterRaw(c)
	r.VT, r.Keys = s.VT, s.Keys
	r.RawIn, _ = c.InMode()
	r.RawOut, _ = c.OutMode()
	r.BigFrameLast = writeBigFrame(c)
	if err := s.Restore(); err != nil {
		r.Err = err.Error()
	}
	r.RestIn, _ = c.InMode()
	r.RestOut, _ = c.OutMode()
	r.Written = writeAndReadBack(c, consoleWriteProbe)
}

// writeBigFrame writes bigFrame (VT on) and reads back the row the
// cursor ends on.
func writeBigFrame(c *WinConsole) string {
	if _, err := c.Write([]byte("\r\n" + bigFrame())); err != nil {
		return "write: " + err.Error()
	}
	var info windows.ConsoleScreenBufferInfo
	if err := windows.GetConsoleScreenBufferInfo(c.out, &info); err != nil {
		return "info: " + err.Error()
	}
	n := len(utf16.Encode([]rune(bigFrameLastRow)))
	buf := make([]uint16, n)
	var read uint32
	proc := windows.NewLazySystemDLL("kernel32.dll").NewProc("ReadConsoleOutputCharacterW")
	pos := uintptr(uint32(uint16(info.CursorPosition.Y)) << 16)
	if r, _, err := proc.Call(uintptr(c.out), uintptr(unsafe.Pointer(&buf[0])), uintptr(n), pos, uintptr(unsafe.Pointer(&read))); r == 0 {
		return "read: " + err.Error()
	}
	_, _ = c.Write([]byte("\r\n"))
	return string(utf16.Decode(buf[:read]))
}

// writeAndReadBack writes s at the cursor with Console.Write and reads the
// characters back from the screen buffer.
func writeAndReadBack(c *WinConsole, s string) string {
	var info windows.ConsoleScreenBufferInfo
	if err := windows.GetConsoleScreenBufferInfo(c.out, &info); err != nil {
		return "info: " + err.Error()
	}
	if _, err := c.Write([]byte(s)); err != nil {
		return "write: " + err.Error()
	}
	n := len(utf16.Encode([]rune(s)))
	buf := make([]uint16, n)
	var read uint32
	proc := windows.NewLazySystemDLL("kernel32.dll").NewProc("ReadConsoleOutputCharacterW")
	pos := uintptr(uint32(uint16(info.CursorPosition.X)) | uint32(uint16(info.CursorPosition.Y))<<16)
	if r, _, err := proc.Call(uintptr(c.out), uintptr(unsafe.Pointer(&buf[0])), uintptr(n), pos, uintptr(unsafe.Pointer(&read))); r == 0 {
		return "read: " + err.Error()
	}
	return string(utf16.Decode(buf[:read]))
}

func TestWinConsole_ProcessCount(t *testing.T) {
	n, err := NewConsole().ProcessCount()
	if err != nil {
		t.Skipf("no console attached to the test process: %v", err)
	}
	// go test runs the binary under the go tool, so it never owns the console alone.
	if n < 2 {
		t.Errorf("ProcessCount = %d, want >= 2 under go test", n)
	}
}

// Read-only: the test must not change the developer's console modes.
func TestWinConsole_ModesAndWidth(t *testing.T) {
	c := NewConsole()
	for name, get := range map[string]func() (uint32, error){"stdin": c.InMode, "stdout": c.OutMode} {
		m, err := get()
		switch {
		case errors.Is(err, ErrNotConsole):
			t.Logf("%s is redirected", name)
		case err != nil:
			t.Errorf("%s mode: unexpected error %v", name, err)
		default:
			t.Logf("%s mode %#x", name, m)
		}
	}
	if w, err := c.Width(); err == nil && w <= 0 {
		t.Errorf("width = %d", w)
	}
	if h, err := c.Height(); err == nil && h <= 0 {
		t.Errorf("height = %d", h)
	}
}

// openConsoleDevices opens CONIN$/CONOUT$ directly, bypassing redirected
// standard handles (used by tests that run in their own console).
func openConsoleDevices() (*WinConsole, func(), error) {
	open := func(name string) (windows.Handle, error) {
		p, _ := windows.UTF16PtrFromString(name)
		return windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	}
	in, err := open("CONIN$")
	if err != nil {
		return nil, nil, err
	}
	out, err := open("CONOUT$")
	if err != nil {
		_ = windows.CloseHandle(in)
		return nil, nil, err
	}
	return &WinConsole{in: in, out: out}, func() { _ = windows.CloseHandle(in); _ = windows.CloseHandle(out) }, nil
}
