//go:build integration && windows

package sysapi

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

const freshConsoleEnv = "NETDIAG_FRESH_CONSOLE_RESULT"

type freshConsoleResult struct {
	ProcessCount    int
	OrigIn, OrigOut uint32
	RawIn, RawOut   uint32
	RestIn, RestOut uint32
	VT, Keys        bool
	Err             string
}

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
	if err := s.Restore(); err != nil {
		r.Err = err.Error()
	}
	r.RestIn, _ = c.InMode()
	r.RestOut, _ = c.OutMode()
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
}
