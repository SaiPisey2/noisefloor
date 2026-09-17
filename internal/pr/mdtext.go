// Package pr, mdText: escaping for remote-sourced strings in a pull
// request body. Kept in its own file because every print site in body.go
// depends on it.
package pr

import (
	"strings"

	"github.com/SaiPisey2/noisefloor/internal/safe"
)

// mdText renders a remote-sourced string (an alert name, a rule group, or the
// author and comment of an Alertmanager silence) for embedding in a pull request body that a
// Markdown renderer will parse. Every one of these strings is untrusted:
// anyone who can name an alerting rule, or write the comment on a silence,
// controls what lands here, and this body's whole
// purpose is to be an approval surface a stranger reads before deleting or
// retuning an alerting rule. A silence comment is the sharpest case: it is
// free text, written by whoever created the silence, and it lands in a list
// item in someone else's repository. A name that renders as a live link or a
// tracking beacon there is a phishing vector, not a cosmetic bug.
//
// safe.Text handles the first problem: any rune that is not printable is
// rendered as its visible escape sequence instead of being emitted. That
// covers control characters -- an ANSI escape, a bare CR/LF, a tab, which
// execute in a terminal or forge a line break inside what is meant to be
// one Markdown table row -- and, just as importantly here, the Unicode
// FORMAT characters that are not control characters at all: U+202E and the
// directional isolates reorder the glyphs after them, so a name can be made
// to DISPLAY as a different name than the one being dropped, and U+200B,
// U+00AD and U+FEFF are invisible outright, so a reviewer cannot see that
// the name in the table is not the name they know. On an approval surface
// for an irreversible deletion, a name that does not read as itself is the
// whole attack.
//
// Markdown and GitHub's raw-HTML pass-through add every other problem
// safe.Text does not touch, so mdText neutralises each construct that turns
// plain text into something that renders as structure rather than content:
//
//   - "|" is escaped because it is the table's own column separator.
//   - "`" is escaped defensively. This body never wraps a remote-sourced
//     string in backticks -- unlike a naive `%s` inline code span, plain
//     text cannot be broken out of, because there is no delimiter of ours
//     for an embedded backtick to pair with. But two backticks in the same
//     table row (this field's and some other field's) could still pair up
//     across cells and swallow whatever sits between them, "|" included,
//     once a Markdown table parser resolves code spans before splitting a
//     row on "|" -- so a literal backtick is escaped anyway.
//   - "[", "]", "(", ")" and "!" are escaped so link and image syntax --
//     "[text](url)" or "![alt](url)" -- can never form. An unescaped image
//     renders as a live, unprompted GET request (a tracking beacon); an
//     unescaped link renders as something a reviewer can click without
//     ever seeing the URL as text.
//   - "<" and ">" are rendered as the HTML entities "&lt;"/"&gt;" rather
//     than backslash-escaped. GitHub's Markdown renders raw HTML that
//     survives inline parsing, so a script tag, a forged `</td><td>`, or an
//     HTML comment hiding real content is a structural break, not just a
//     display glitch. A backslash escape ("\<") depends on every renderer
//     in this body's path doing CommonMark-correct backslash-escape
//     processing before HTML-tag recognition; an entity reference does not
//     depend on that at all; it is inert text at the point angle brackets
//     would otherwise start being read as a tag; that holds regardless of
//     what parses this Markdown next.
//   - "." and "@" are rendered as the HTML entities "&#46;"/"&#64;". None
//     of the delimiter escaping above stops GitHub Flavored Markdown's
//     *extended autolink*, which needs no delimiters at all: bare
//     "www.evil.example", "http://evil.example" and "me@evil.example"
//     become live links purely from their own characters. A silence comment is exactly the kind of
//     free-form string that can spell one of these out, turning this approval surface into a phishing
//     link or a mailto: a reviewer can click without ever deciding to.
//     Entity references, like the "<"/"&gt;" ones above, render as the
//     original character but leave no literal "." or "@" for the autolink
//     scanner to anchor on.
//
// Order matters throughout: backslashes are escaped first, so a backslash
// already escaping one of these characters can never be produced by this
// function's own escaping and then re-interpreted as escaping something
// else. "&" is escaped immediately after, and before every entity this
// function introduces ("&lt;", "&gt;", "&#46;", "&#64;"): escaping it
// first, rather than not touching it at all, closes two problems with one
// rule. First, it stops this function's own output from being
// double-escaped -- if "&" escaping ran after the "<"/"." replacements
// below, "&lt;" would become "&amp;lt;", which renders as the literal
// text "&lt;" instead of "<". Second, it stops a remote string from
// smuggling a literal "." or "@" past the checks below by spelling out
// its own entity: an input already containing the literal text "&#46;"
// would, left untouched, still decode to "." in a Markdown/HTML renderer
// even though this function never wrote a "." itself. Escaping any
// pre-existing "&" to "&amp;" first makes that sequence render as the
// inert text "&#46;", not a decoded period.
func mdText(s string) string {
	s = safe.Text(s)
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "|", `\|`)
	s = strings.ReplaceAll(s, "`", "\\`")
	s = strings.ReplaceAll(s, "[", `\[`)
	s = strings.ReplaceAll(s, "]", `\]`)
	s = strings.ReplaceAll(s, "(", `\(`)
	s = strings.ReplaceAll(s, ")", `\)`)
	s = strings.ReplaceAll(s, "!", `\!`)
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, ".", "&#46;")
	s = strings.ReplaceAll(s, "@", "&#64;")
	return s
}
