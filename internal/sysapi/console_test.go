package sysapi

import (
	"context"
	"testing"
	"time"
)

// Typical cooked-mode defaults of a Windows console.
const (
	defaultIn  = ModeProcessedInput | ModeLineInput | ModeEchoInput | ModeQuickEdit | ModeExtendedFlags | 0x0010 | 0x0020 // + mouse, insert
	defaultOut = ModeProcessedOutput | 0x0002                                                                             // + wrap at EOL
)

func TestRawInputMode(t *testing.T) {
	t.Parallel()
	tests := []uint32{defaultIn, 0, ModeLineInput | ModeEchoInput, ModeQuickEdit}
	for _, orig := range tests {
		got := RawInputMode(orig)
		if got&(ModeLineInput|ModeEchoInput|ModeQuickEdit) != 0 {
			t.Errorf("RawInputMode(%#x) = %#x: line/echo/quick-edit still set", orig, got)
		}
		if got&ModeProcessedInput == 0 {
			t.Errorf("RawInputMode(%#x) = %#x: processed input cleared, Ctrl+C would not signal", orig, got)
		}
		if got&ModeExtendedFlags == 0 {
			t.Errorf("RawInputMode(%#x) = %#x: extended flags missing, quick-edit change would not apply", orig, got)
		}
	}
	if got := RawInputMode(defaultIn); got&0x0030 != 0x0030 {
		t.Errorf("unrelated bits must be kept, got %#x", got)
	}
}

func TestVTOutputMode(t *testing.T) {
	t.Parallel()
	if got := VTOutputMode(defaultOut); got != defaultOut|ModeVTProcessing {
		t.Errorf("VTOutputMode = %#x", got)
	}
}

func TestEnterRaw_RestoresOriginalModes(t *testing.T) {
	t.Parallel()
	c := &FakeConsole{In: defaultIn, Out: defaultOut}
	s := EnterRaw(c)
	if !s.VT || !s.Keys {
		t.Fatalf("VT=%v Keys=%v, want both", s.VT, s.Keys)
	}
	if c.In != RawInputMode(defaultIn) || c.Out != VTOutputMode(defaultOut) {
		t.Errorf("raw modes not applied: in=%#x out=%#x", c.In, c.Out)
	}
	if err := s.Restore(); err != nil {
		t.Fatal(err)
	}
	if c.In != defaultIn || c.Out != defaultOut {
		t.Errorf("not restored: in=%#x out=%#x", c.In, c.Out)
	}
	for _, ch := range c.ModeChanges() {
		if ch.Stream == "in" && ch.Mode&ModeProcessedInput == 0 {
			t.Errorf("ENABLE_PROCESSED_INPUT was cleared in change %+v", ch)
		}
	}
}

func TestEnterRaw_RestoreIsIdempotent(t *testing.T) {
	t.Parallel()
	c := &FakeConsole{In: defaultIn, Out: defaultOut}
	s := EnterRaw(c)
	_ = s.Restore()
	_ = s.Restore()
	if n := len(c.ModeChanges()); n != 4 {
		t.Errorf("want 4 mode changes (2 enter, 2 restore), got %d: %+v", n, c.ModeChanges())
	}
}

func TestEnterRaw_RedirectedHandlesAreLeftAlone(t *testing.T) {
	t.Parallel()
	c := &FakeConsole{NoIn: true, NoOut: true}
	s := EnterRaw(c)
	if s.VT || s.Keys {
		t.Error("redirected handles must not report VT or keys")
	}
	if err := s.Restore(); err != nil || len(c.ModeChanges()) != 0 {
		t.Errorf("no mode change expected, got %+v, %v", c.ModeChanges(), err)
	}
}

func TestEnterRaw_StdinRedirectedStdoutConsole(t *testing.T) {
	t.Parallel()
	c := &FakeConsole{NoIn: true, Out: defaultOut}
	s := EnterRaw(c)
	if !s.VT || s.Keys {
		t.Errorf("VT=%v Keys=%v, want VT only", s.VT, s.Keys)
	}
	_ = s.Restore()
	if c.Out != defaultOut {
		t.Errorf("stdout not restored: %#x", c.Out)
	}
}

func TestEnterRaw_VTUnsupported(t *testing.T) {
	t.Parallel()
	c := &FakeConsole{In: defaultIn, Out: defaultOut, FailSetOut: true}
	s := EnterRaw(c)
	if s.VT || !s.Keys {
		t.Errorf("VT=%v Keys=%v, want keys only", s.VT, s.Keys)
	}
	_ = s.Restore()
	if c.In != defaultIn {
		t.Errorf("stdin not restored: %#x", c.In)
	}
	for _, ch := range c.ModeChanges() {
		if ch.Stream == "out" {
			t.Errorf("stdout must not be touched when VT failed: %+v", ch)
		}
	}
}

func TestFakeConsole_ReadKeyHonoursContext(t *testing.T) {
	t.Parallel()
	c := &FakeConsole{Keys: make(chan Key, 1)}
	c.Keys <- Key{Rune: 'q'}
	if k, err := c.ReadKey(context.Background()); err != nil || k.Rune != 'q' {
		t.Fatalf("got %+v, %v", k, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := c.ReadKey(ctx); err == nil {
		t.Error("want context error")
	}
}
