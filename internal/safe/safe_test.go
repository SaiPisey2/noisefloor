package safe

import (
	"fmt"
	"testing"
)

// cp renders one code point as the literal character, so a test case can
// name a deceptive rune by its number rather than embedding a glyph that
// is, by definition, invisible or misleading in a source file.
func cp(r rune) string { return string(r) }

// wantU is the escape sequence Text is expected to produce for r, and
// wantX the one it is expected to produce for a raw byte.
func wantU(r rune) string { return fmt.Sprintf("\\u%04x", r) }
func wantX(b byte) string { return fmt.Sprintf("\\x%02x", b) }

func TestText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "ANSI escape is rendered visibly",
			in:   "up\x1b[2K\x1b[1;31mFAKE\x1b[0m",
			want: `up\x1b[2K\x1b[1;31mFAKE\x1b[0m`,
		},
		{
			name: "CRLF is rendered visibly",
			in:   "up\r\nnode_cpu_seconds_total",
			want: `up\r\nnode_cpu_seconds_total`,
		},
		{
			name: "tab is rendered visibly",
			in:   "up\ttab",
			want: `up\ttab`,
		},
		{
			name: "plain ASCII is unchanged",
			in:   "node_cpu_seconds_total",
			want: "node_cpu_seconds_total",
		},
		{
			name: "UTF-8 is unchanged",
			in:   "café_total",
			want: "café_total",
		},
		{
			// U+202E RIGHT-TO-LEFT OVERRIDE reverses the glyphs that
			// follow it, so this name DISPLAYS as one thing while naming
			// another. The whole point of the PR body is that a human
			// reads the name before approving its permanent deletion.
			// unicode.IsControl does not cover it; unicode.IsPrint does.
			name: "right-to-left override is rendered visibly",
			in:   "cpu_total" + cp(0x202E) + "latot_ksid" + cp(0x202C),
			want: "cpu_total" + wantU(0x202E) + "latot_ksid" + wantU(0x202C),
		},
		{
			name: "directional isolates are rendered visibly",
			in:   "a" + cp(0x2066) + "b" + cp(0x2067) + "c" + cp(0x2069) + "d",
			want: "a" + wantU(0x2066) + "b" + wantU(0x2067) + "c" + wantU(0x2069) + "d",
		},
		{
			// Invisible outright: without this, two different metric names
			// render identically and a reviewer cannot tell them apart.
			name: "zero width space is rendered visibly",
			in:   "http_requests" + cp(0x200B) + "_total",
			want: "http_requests" + wantU(0x200B) + "_total",
		},
		{
			name: "soft hyphen and BOM are rendered visibly",
			in:   "up" + cp(0x00AD) + "time" + cp(0xFEFF),
			want: "up" + wantU(0x00AD) + "time" + wantU(0xFEFF),
		},
		{
			name: "line and paragraph separators are rendered visibly",
			in:   "a" + cp(0x2028) + "b" + cp(0x2029) + "c",
			want: "a" + wantU(0x2028) + "b" + wantU(0x2029) + "c",
		},
		{
			// A non-breaking space masquerades as an ordinary one.
			name: "non-breaking space is rendered visibly",
			in:   "node" + cp(0x00A0) + "cpu",
			want: "node" + wantU(0x00A0) + "cpu",
		},
		{
			// Emitting the byte would hand the terminal something it
			// cannot decode; silently substituting U+FFFD would rewrite
			// what the remote actually sent.
			name: "invalid UTF-8 is rendered as hex",
			in:   "up\xff\xfeok",
			want: "up" + wantX(0xff) + wantX(0xfe) + "ok",
		},
		{
			name: "CJK is unchanged",
			in:   "指標_total",
			want: "指標_total",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Text(tc.in); got != tc.want {
				t.Errorf("Text(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
