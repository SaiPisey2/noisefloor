package pr

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/SaiPisey2/noisefloor/internal/store"
)

// cp renders one code point as the literal character, so a case can name a
// deceptive rune by its number rather than embedding a glyph that is, by
// definition, invisible or misleading in a source file.
func cp(r rune) string { return string(r) }

func TestMdTextNeutralisesMarkdownStructure(t *testing.T) {
	cases := []struct {
		name string
		in   string
		bad  []string
	}{
		{
			name: "an image is not left renderable",
			in:   "![x](https://evil.example/beacon.png)",
			bad:  []string{"](", "!["},
		},
		{
			name: "a link is not left renderable",
			in:   "[click here](https://evil.example)",
			bad:  []string{"]("},
		},
		{
			name: "raw HTML is inert",
			in:   `<img src=x onerror=alert(1)>`,
			bad:  []string{"<img", ">"},
		},
		{
			name: "a table cell cannot be split",
			in:   "a | b | c",
			bad:  []string{" | "},
		},
		{
			// GitHub's extended autolink needs no delimiters at all.
			name: "a bare host does not autolink",
			in:   "www.evil.example",
			bad:  []string{"www.evil"},
		},
		{
			name: "a bare address does not autolink",
			in:   "oncall@evil.example",
			bad:  []string{"@"},
		},
		{
			name: "an entity cannot be smuggled in to decode later",
			in:   "www&#46;evil&#46;example",
			bad:  []string{"www&#46;evil"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mdText(tc.in)
			for _, b := range tc.bad {
				if strings.Contains(got, b) {
					t.Errorf("mdText(%q) = %q, still contains %q", tc.in, got, b)
				}
			}
		})
	}
}

// TestRetireBodyEscapesEveryRemoteString is the body-level check. The alert
// name and group come from whoever writes the alerting rules; a silence's
// author and comment come from whoever created the silence. All four land
// in a pull request in someone else's repository.
func TestRetireBodyEscapesEveryRemoteString(t *testing.T) {
	e := Evidence{
		Group:             "grp](https://evil.example)",
		AlertName:         "Disk" + cp(0x202E) + "lacitirc" + cp(0x202C),
		Fires:             10,
		Confidence:        0.9,
		WindowStart:       time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC),
		WindowEnd:         time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC),
		SilencesAvailable: true,
		SilencedBy: []store.Silence{{
			CreatedBy: "ops@evil.example",
			Comment:   "see ![](https://evil.example/beacon.png) | and www.evil.example",
			StartsAt:  time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
			EndsAt:    time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC),
		}},
	}
	body := RetireBody(e)

	// Nothing that renders as structure rather than content.
	for _, bad := range []string{"](", "![", "www.evil", "ops@evil"} {
		if strings.Contains(body, bad) {
			t.Errorf("body still contains %q, which renders as something other than text:\n%s", bad, body)
		}
	}
	// A pipe inside a value must not be able to split a table row.
	if strings.Contains(body, "beacon.png) | and") {
		t.Errorf("an unescaped pipe survived into the body:\n%s", body)
	}
	// And nothing that reorders or hides glyphs.
	for _, r := range []rune{0x202E, 0x202C} {
		if strings.ContainsRune(body, r) {
			t.Errorf("body carries U+%04X verbatim; it must render as a visible escape", r)
		}
		if !strings.Contains(body, fmt.Sprintf("\\u%04x", r)) {
			t.Errorf("body does not render U+%04X visibly", r)
		}
	}
}
