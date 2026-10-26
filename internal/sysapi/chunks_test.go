package sysapi

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"
)

func u16(s string) []uint16 { return utf16.Encode([]rune(s)) }

// checkChunks asserts the invariants every split must keep.
func checkChunks(t *testing.T, in []uint16, chunks [][]uint16, limit int) {
	t.Helper()
	var joined []uint16
	for i, c := range chunks {
		if len(c) == 0 || len(c) > limit {
			t.Errorf("chunk %d has %d units (limit %d)", i, len(c), limit)
		}
		if last := c[len(c)-1]; last >= 0xD800 && last <= 0xDBFF {
			t.Errorf("chunk %d ends with a high surrogate %#x", i, last)
		}
		joined = append(joined, c...)
	}
	if !slices.Equal(joined, in) {
		t.Error("chunks do not concatenate to the input")
	}
}

func TestSplitChunks_EmptyAndExact(t *testing.T) {
	t.Parallel()
	if got := splitChunks(nil, 8); len(got) != 0 {
		t.Errorf("empty input: %d chunks", len(got))
	}
	in := u16(strings.Repeat("a", 24)) // no newline: hard splits at exact multiples
	got := splitChunks(in, 8)
	checkChunks(t, in, got, 8)
	if len(got) != 3 {
		t.Errorf("24 units at 8: %d chunks", len(got))
	}
	in = u16("abcdefgh")
	if got := splitChunks(in, 8); len(got) != 1 {
		t.Errorf("exactly the limit: %d chunks", len(got))
	}
}

// An emoji (surrogate pair) at the boundary stays in one chunk.
func TestSplitChunks_SurrogatePairAtBoundary(t *testing.T) {
	t.Parallel()
	const limit = 8
	for _, at := range []int{limit - 2, limit - 1} { // pair at max-2/max-1 and max-1/max
		in := append(u16(strings.Repeat("a", at)), u16("😀bbbbbbbbbb")...)
		got := splitChunks(in, limit)
		checkChunks(t, in, got, limit)
		if at == limit-1 && len(got[0]) != limit-1 {
			t.Errorf("pair at %d: first chunk %d units, want %d", at, len(got[0]), limit-1)
		}
		if at == limit-2 && len(got[0]) != limit {
			t.Errorf("pair at %d: first chunk %d units, want %d (the whole pair fits)", at, len(got[0]), limit)
		}
	}
}

// A frame is split only after a line break, so no VT sequence is cut.
func TestSplitChunks_FrameSplitsAfterNewline(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	for i := range 400 {
		fmt.Fprintf(&b, "\x1b[H\x1b[1;32mrow %03d\x1b[0m %s\x1b[K\r\n", i, strings.Repeat("▁▂▃", 10))
	}
	in := u16(b.String())
	const limit = 1000
	got := splitChunks(in, limit)
	checkChunks(t, in, got, limit)
	for i, c := range got[:len(got)-1] {
		if c[len(c)-1] != '\n' {
			t.Errorf("chunk %d ends with %q, want a line break", i, rune(c[len(c)-1]))
		}
	}
}

func TestSplitChunks_LineLongerThanLimit(t *testing.T) {
	t.Parallel()
	in := u16("short\n" + strings.Repeat("x", 25) + "\nend")
	got := splitChunks(in, 10)
	checkChunks(t, in, got, 10)
	if string(utf16.Decode(got[0])) != "short\n" {
		t.Errorf("first chunk %q, want the first line", string(utf16.Decode(got[0])))
	}
}
