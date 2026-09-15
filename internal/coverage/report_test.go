package coverage

import "testing"

// TestCellDistinguishesGlobalFromMatched pins the grid-glyph fix: matched
// and global coverage used to render identically ("v" for either), which
// made a service the blind-spot list names as uncovered read as "v" (fully
// covered) in the grid above it -- the same underlying fact (a cluster-wide
// `up == 0`) answering AnyCoverage and AnyScopedCoverage differently while
// showing one indistinguishable glyph.
func TestCellDistinguishesGlobalFromMatched(t *testing.T) {
	cases := []struct {
		name    string
		matches []RuleMatch
		want    string
	}{
		{"none", nil, "-"},
		{"matched certain", []RuleMatch{{Scope: "matched", Certain: true}}, "v"},
		{"matched guess", []RuleMatch{{Scope: "matched", Certain: false}}, "v?"},
		{"global certain", []RuleMatch{{Scope: "global", Certain: true}}, "g"},
		{"global guess", []RuleMatch{{Scope: "global", Certain: false}}, "g?"},
		{
			"matched certain beats global certain",
			[]RuleMatch{{Scope: "global", Certain: true}, {Scope: "matched", Certain: true}},
			"v",
		},
		{
			"global certain beats matched guess",
			[]RuleMatch{{Scope: "matched", Certain: false}, {Scope: "global", Certain: true}},
			"g",
		},
	}
	for _, c := range cases {
		sc := ServiceCoverage{Covered: map[Signal][]RuleMatch{SignalRate: c.matches}}
		if got := cell(sc, SignalRate); got != c.want {
			t.Errorf("%s: cell() = %q, want %q", c.name, got, c.want)
		}
	}
}
