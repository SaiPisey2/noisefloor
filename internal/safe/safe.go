// Package safe renders remote-sourced strings for output a human or a
// Markdown renderer trusts to be well-formed.
package safe

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const hexDigits = "0123456789abcdef"

// Text renders s safely for output that a human reads. Metric and rule
// names come from a remote Prometheus or Alertmanager and are not trusted: anyone who can
// expose a metric to a scraped target chooses what lands here, and what
// lands here is read by someone deciding whether to permanently stop
// storing it.
//
// Every rune that is not printable is rendered as its visible escape
// sequence instead of being emitted, and every byte that is not valid
// UTF-8 is rendered as \xNN. "Not printable" is unicode.IsPrint, which is
// deliberately wider than unicode.IsControl:
//
//   - C0/C1 controls (\x1b, \r, \n, \t). An ANSI escape can clear or forge
//     a terminal line; a bare newline can forge a row in a Markdown table.
//   - Format characters (category Cf), which IsControl does NOT cover.
//     U+202E RIGHT-TO-LEFT OVERRIDE and the U+2066..U+2069 isolates
//     reorder the glyphs after them, so a name can be made to DISPLAY as
//     a different name than the one being dropped -- the Trojan Source
//     shape, pointed at an approval surface. U+200B ZERO WIDTH SPACE,
//     U+00AD SOFT HYPHEN and U+FEFF are invisible outright, so a reviewer
//     cannot see that the name in the table is not the name they know.
//   - Non-ASCII separators and spaces (U+2028, U+2029, U+00A0), which
//     break lines or masquerade as an ordinary space.
//
// Ordinary non-ASCII text is untouched: IsPrint accepts letters, marks,
// numbers, punctuation, symbols and the ASCII space, so a metric name
// carrying accented or CJK characters -- legitimate under Prometheus 3's
// UTF-8 naming scheme -- still renders as itself.
func Text(s string) string {
	if clean(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			// An invalid UTF-8 byte. Writing it through would hand the
			// terminal (or the Markdown renderer) a byte neither can
			// interpret, and letting DecodeRune's U+FFFD stand in its
			// place would silently rewrite what the remote actually sent.
			b.WriteString(`\x`)
			b.WriteByte(hexDigits[s[i]>>4])
			b.WriteByte(hexDigits[s[i]&0x0f])
			i++
			continue
		}
		if unicode.IsPrint(r) {
			b.WriteString(s[i : i+size])
		} else {
			// strconv.QuoteRune escapes any non-printable rune into a
			// visible form: '\x1b', '\n', '‮'. Strip the surrounding
			// rune quotes to get just the escape sequence.
			q := strconv.QuoteRune(r)
			b.WriteString(q[1 : len(q)-1])
		}
		i += size
	}
	return b.String()
}

// clean reports whether s is entirely valid UTF-8 and entirely printable,
// so the common case returns the original string without allocating.
func clean(s string) bool {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			return false
		}
		if !unicode.IsPrint(r) {
			return false
		}
		i += size
	}
	return true
}
