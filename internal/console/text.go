package console

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// clean makes untrusted text safe to print (SSIDs, reverse DNS names,
// adapter names, targets, error details): invalid UTF-8, C0 controls
// (incl. ESC, CR, LF, TAB, NUL), DEL, C1 controls, bidi overrides and
// isolates, and runes outside the BMP (emoji render with inconsistent
// widths) each become "?". Anything else is kept.
func clean(s string) string {
	ok := true
	for i, r := range s {
		if unsafeRune(r) || (r == utf8.RuneError && !validAt(s, i)) {
			ok = false
			break
		}
	}
	if ok {
		return s
	}
	var b strings.Builder
	for i, r := range s {
		if unsafeRune(r) || (r == utf8.RuneError && !validAt(s, i)) {
			b.WriteByte('?')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// validAt reports whether s holds a real U+FFFD at byte i (not a byte of
// invalid UTF-8, which ranging over s also yields as RuneError).
func validAt(s string, i int) bool {
	_, size := utf8.DecodeRuneInString(s[i:])
	return size > 1
}

func unsafeRune(r rune) bool {
	switch {
	case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f: // C0 (ESC, CR, LF, TAB, NUL), DEL, C1
		return true
	case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069: // bidi embeddings, overrides, isolates
		return true
	case r > 0xffff:
		return true
	}
	return false
}

// displayWidth is the number of console columns a rune takes: 0 for
// combining marks and format characters (ZWJ, variation selectors), 2 for
// East Asian wide and fullwidth characters, 1 otherwise. A small table
// instead of golang.org/x/text (not on the dependency allowlist).
func displayWidth(r rune) int {
	switch {
	case unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf):
		return 0
	case r >= 0x1100 && r <= 0x115f, // Hangul Jamo initials
		r >= 0x2e80 && r <= 0xa4cf, // CJK radicals … Yi
		r >= 0xac00 && r <= 0xd7a3, // Hangul syllables
		r >= 0xf900 && r <= 0xfaff, // CJK compatibility ideographs
		r >= 0xfe30 && r <= 0xfe6f, // CJK compatibility forms, small forms
		r >= 0xff00 && r <= 0xff60, // fullwidth forms
		r >= 0xffe0 && r <= 0xffe6:
		return 2
	}
	return 1
}

// stringWidth is the display width of s.
func stringWidth(s string) int {
	n := 0
	for _, r := range s {
		n += displayWidth(r)
	}
	return n
}

// cutWidth returns the longest prefix of s at most w columns wide (keeping
// combining marks with their base) and its width. A wide rune that does
// not fit whole is left out.
func cutWidth(s string, w int) (string, int) {
	n := 0
	for i, r := range s {
		rw := displayWidth(r)
		if n+rw > w {
			return s[:i], n
		}
		n += rw
	}
	return s, n
}
