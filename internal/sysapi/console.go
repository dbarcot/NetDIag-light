// Package sysapi defines interfaces for OS information and console access,
// with Windows implementations and fakes for tests.
package sysapi

import (
	"context"
	"errors"
	"sync"
)

// Console mode flags (wincon.h).
const (
	// stdin
	ModeProcessedInput uint32 = 0x0001 // Ctrl+C raises a signal
	ModeLineInput      uint32 = 0x0002
	ModeEchoInput      uint32 = 0x0004
	ModeQuickEdit      uint32 = 0x0040
	ModeExtendedFlags  uint32 = 0x0080 // required for the quick-edit bit to apply
	// stdout
	ModeProcessedOutput uint32 = 0x0001
	ModeVTProcessing    uint32 = 0x0004 // ENABLE_VIRTUAL_TERMINAL_PROCESSING
)

// Key is one key-down event read from the console.
type Key struct {
	Rune       rune   // 0 for keys without a character
	VirtualKey uint16 // VK_* code
	Control    uint32 // dwControlKeyState
}

// Console is access to the process's console. Mode getters return an error
// wrapping ErrNotConsole when the handle is redirected.
type Console interface {
	// ProcessCount returns how many processes are attached to the console.
	// 1 means the console was created for this process alone, e.g. by a
	// double-click in Explorer. An error means there is no console.
	ProcessCount() (int, error)
	InMode() (uint32, error)
	SetInMode(mode uint32) error
	OutMode() (uint32, error)
	SetOutMode(mode uint32) error
	// Width is the visible console width in columns.
	Width() (int, error)
	// ReadKey blocks until a key-down event arrives or ctx ends. Non-key
	// events and key-up events are skipped.
	ReadKey(ctx context.Context) (Key, error)
}

// RawInputMode returns the stdin mode for live key input (SPEC §9): no line
// input, no echo, no quick edit; extended flags and processed input set.
func RawInputMode(orig uint32) uint32 {
	return orig&^(ModeLineInput|ModeEchoInput|ModeQuickEdit) | ModeExtendedFlags | ModeProcessedInput
}

// VTOutputMode returns the stdout mode with VT sequences enabled.
func VTOutputMode(orig uint32) uint32 {
	return orig | ModeProcessedOutput | ModeVTProcessing
}

// RawSession holds the original console modes and restores them.
type RawSession struct {
	c             Console
	in, out       uint32
	inSet, outSet bool
	once          sync.Once
	restoreErr    error

	VT   bool // stdout accepts VT sequences
	Keys bool // stdin is a console in raw mode; ReadKey can be used
}

// EnterRaw saves both console modes, enables VT output and raw key input
// where the handle is a console, and returns a session whose Restore puts
// the original modes back. Redirected handles are left alone.
func EnterRaw(c Console) *RawSession {
	s := &RawSession{c: c}
	if out, err := c.OutMode(); err == nil {
		s.out = out
		if c.SetOutMode(VTOutputMode(out)) == nil {
			s.outSet, s.VT = true, true
		}
	}
	if in, err := c.InMode(); err == nil {
		s.in = in
		if c.SetInMode(RawInputMode(in)) == nil {
			s.inSet, s.Keys = true, true
		}
	}
	return s
}

// Restore sets the saved modes back. It is safe to call more than once and
// from several exit paths; only the first call acts.
func (s *RawSession) Restore() error {
	s.once.Do(func() {
		var errs []error
		if s.inSet {
			errs = append(errs, s.c.SetInMode(s.in))
		}
		if s.outSet {
			errs = append(errs, s.c.SetOutMode(s.out))
		}
		s.restoreErr = errors.Join(errs...)
	})
	return s.restoreErr
}
