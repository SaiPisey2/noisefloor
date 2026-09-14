package pr

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/SaiPisey2/noisefloor/internal/remediate"
	"github.com/SaiPisey2/noisefloor/internal/scanner"
	"github.com/SaiPisey2/noisefloor/internal/score"
)

// Kind is which shape of remediation a Proposal is.
type Kind string

const (
	KindRetire Kind = "retire"
	KindTune   Kind = "tune"
)

// Proposal is one rule's remediation, ready to open as a PR: a deterministic
// branch, a title, a body a reviewer can check unassisted, and the exact
// edit to make -- never more than the one rule it is about.
type Proposal struct {
	Kind             Kind
	Group, AlertName string
	File             string

	Branch string
	Title  string
	Body   string

	Edit Edit

	// BaseContent, NewContent and Diff are populated by Render, not Build:
	// Build only describes the edit, Render is what actually reads the file
	// and applies it (see Edit.Apply). Kept on Proposal so a caller renders
	// once and reuses the result for both the dry-run printout and, with
	// -apply, the commit.
	//
	// BaseContent is the file exactly as it was when this edit was computed.
	// It travels with the proposal all the way to the forge, where it is
	// the answer to "what did this diff assume it was editing" -- the only
	// thing that makes NewContent safe to PUT as a whole file.
	BaseContent string
	NewContent  string
	Diff        string
}

// Render applies p.Edit against the file on disk and fills BaseContent,
// NewContent and Diff. Safe to call more than once; it re-reads the file
// each time, so it always reflects the file's current on-disk state.
func (p *Proposal) Render() error {
	oldContent, newContent, diff, err := p.Edit.Apply()
	if err != nil {
		return err
	}
	p.BaseContent, p.NewContent, p.Diff = oldContent, newContent, diff
	return nil
}

// RefusalReason names why a rule cannot get a proposal. The string value is
// what a dry-run printout and a golden test both show, so it is written as
// a complete clause, not a code.
type RefusalReason string

const (
	ReasonAmbiguous     RefusalReason = "ambiguous: alert name is defined in more than one rule group"
	ReasonInactive      RefusalReason = "inactive: prometheus no longer evaluates this rule"
	ReasonRetuned       RefusalReason = "retuned inside the observation window"
	ReasonLowConfidence RefusalReason = "confidence is below the floor"
	ReasonNoLocation    RefusalReason = "rule location is unknown (rules.path not configured, or the rule was not found under it)"
)

// Refusal is why Build declined to propose a change for one rule.
type Refusal struct {
	Group, AlertName string
	Reason           RefusalReason
	// Detail adds the specific numbers behind Reason, when there are any
	// (e.g. the confidence value against the floor). Empty otherwise.
	Detail string
}

func (r Refusal) String() string {
	if r.Detail != "" {
		return fmt.Sprintf("%s/%s: refused -- %s (%s)", r.Group, r.AlertName, r.Reason, r.Detail)
	}
	return fmt.Sprintf("%s/%s: refused -- %s", r.Group, r.AlertName, r.Reason)
}

// Build turns one scored rule into either a Proposal or a Refusal.
//
// Both return values nil means there is nothing to do: the rule's verdict
// is keep or automate, and none of the refusal conditions apply either --
// this is not reported as a refusal, because a fine rule is not a rule
// noisefloor declined to fix, it is a rule with nothing to fix.
//
// Order matters, and matches the disqualifying conditions in the order
// issue #8 lists them: ambiguous, inactive and retuned rules never carry a
// retire/tune verdict from internal/scanner in the first place (their
// Verdict is forced or left unset), so they are checked before the verdict
// even is -- otherwise they would silently fall into "nothing to do"
// instead of being named. The confidence-floor check sits after the
// verdict check and is unreachable in production as of this scanner
// (score.Verdict already refuses any verdict but keep below the floor,
// which routes here through Verdict != retire/tune) -- it stays as an
// explicit, independently testable refusal because issue #8 names it as
// its own condition, and because scanner and score are not guaranteed to
// stay coupled that tightly forever.
func Build(eval scanner.RuleEval) (*Proposal, *Refusal, error) {
	group, name := eval.Rule.GroupName, eval.Rule.AlertName

	switch {
	case eval.Ambiguous:
		return nil, &Refusal{group, name, ReasonAmbiguous, ""}, nil
	case !eval.Rule.Active:
		return nil, &Refusal{group, name, ReasonInactive, ""}, nil
	case eval.Retuned:
		return nil, &Refusal{group, name, ReasonRetuned, ""}, nil
	}

	if eval.Verdict != score.VerdictRetire && eval.Verdict != score.VerdictTune {
		return nil, nil, nil
	}

	if eval.Confidence < score.MinConfidence {
		return nil, &Refusal{group, name, ReasonLowConfidence,
			fmt.Sprintf("%.2f < %.2f", eval.Confidence, score.MinConfidence)}, nil
	}
	if !eval.HasLocation {
		return nil, &Refusal{group, name, ReasonNoLocation, ""}, nil
	}

	switch eval.Verdict {
	case score.VerdictRetire:
		return buildRetire(eval)
	case score.VerdictTune:
		return buildTune(eval)
	default:
		return nil, nil, nil
	}
}

func evidenceFrom(eval scanner.RuleEval) Evidence {
	return Evidence{
		Group: eval.Rule.GroupName, AlertName: eval.Rule.AlertName,
		Fires:          eval.Signals.Fires,
		ShortLivedRate: eval.Signals.ShortLivedRate,
		SilencedRate:   eval.Signals.SilencedRate,
		Confidence:     eval.Confidence,
		WindowStart:    eval.WindowStart, WindowEnd: eval.WindowEnd,
		SilencedBy: eval.SilencedBy,
	}
}

func buildRetire(eval scanner.RuleEval) (*Proposal, *Refusal, error) {
	loc := eval.Location
	body := RetireBody(evidenceFrom(eval))
	branch := BranchName(KindRetire, eval.Rule.GroupName, eval.Rule.AlertName, "delete")

	return &Proposal{
		Kind: KindRetire, Group: eval.Rule.GroupName, AlertName: eval.Rule.AlertName,
		File:   loc.File,
		Branch: branch,
		Title:  fmt.Sprintf("noisefloor: retire %s", eval.Rule.AlertName),
		Body:   body,
		Edit:   Edit{File: loc.File, DeleteStart: loc.StartLine, DeleteEnd: loc.EndLine},
	}, nil, nil
}

func buildTune(eval scanner.RuleEval) (*Proposal, *Refusal, error) {
	loc := eval.Location
	currentFor := eval.Rule.For

	// SelectCandidateFor's heuristic (currentFor + P90 firing duration) is
	// computed here from Signals.P90Duration directly rather than by
	// calling remediate.SelectCandidateFor again over raw durations: P90 of
	// eval.Durations and eval.Signals.P90Duration are the same percentile
	// call over the same data (see score/signals.go's percentile and
	// remediate/counterfactual.go's SelectCandidateFor), so recomputing it
	// would be deriving a number this package already has.
	candidateFor := currentFor + eval.Signals.P90Duration

	longThreshold := remediate.DefaultLongThreshold(candidateFor)
	cf := remediate.Compute(eval.Durations, currentFor, candidateFor, longThreshold)
	sentence := remediate.Sentence(eval.Signals.P90Duration, cf)

	edit, err := tuneEdit(loc, candidateFor)
	if err != nil {
		return nil, nil, err
	}

	body := TuneBody(evidenceFrom(eval), currentFor, candidateFor, sentence)
	fingerprint := formatDuration(candidateFor)
	branch := BranchName(KindTune, eval.Rule.GroupName, eval.Rule.AlertName, fingerprint)

	return &Proposal{
		Kind: KindTune, Group: eval.Rule.GroupName, AlertName: eval.Rule.AlertName,
		File:   loc.File,
		Branch: branch,
		Title:  fmt.Sprintf("noisefloor: tune %s", eval.Rule.AlertName),
		Body:   body,
		Edit:   edit,
	}, nil, nil
}

// tuneEdit builds the Edit for a tune proposal: replace the existing
// `for:` line's value in place (preserving indentation and any trailing
// comment) when the rule has one, or insert a new `for:` line when it does
// not.
func tuneEdit(loc remediate.RuleLocation, candidateFor time.Duration) (Edit, error) {
	if loc.ForLine > 0 {
		original, err := readLine(loc.File, loc.ForLine)
		if err != nil {
			return Edit{}, err
		}
		replaced, err := replaceForValue(original, candidateFor)
		if err != nil {
			return Edit{}, fmt.Errorf("%s:%d: %w", loc.File, loc.ForLine, err)
		}
		return Edit{File: loc.File, ReplaceLine: loc.ForLine, ReplaceWith: replaced}, nil
	}

	// No existing for: line. Insert one right after expr: (or, lacking
	// that, right after the rule's own first line), indented to match the
	// field it follows.
	anchor := loc.ExprEndLine
	template := loc.ExprLine
	if anchor == 0 {
		anchor = loc.StartLine
		template = loc.StartLine
	}
	indentSrc, err := readLine(loc.File, template)
	if err != nil {
		return Edit{}, err
	}
	newLine := fieldIndent(indentSrc, template == loc.StartLine) + "for: " + formatDuration(candidateFor)
	return NewInsert(loc.File, anchor, newLine), nil
}

// readLine returns the exact text of one 1-indexed line of path, with no
// trailing newline.
func readLine(path string, lineNo int) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if lineNo < 1 || lineNo > len(lines) {
		return "", fmt.Errorf("%s: line %d out of range (file has %d lines)", path, lineNo, len(lines))
	}
	return lines[lineNo-1], nil
}

// replaceForValue rewrites an existing `for:` line's value in place,
// preserving everything else about the line byte-for-byte: leading
// indentation, the spacing before the value, and any trailing content
// (typically a `# comment`) after it.
func replaceForValue(original string, candidate time.Duration) (string, error) {
	const key = "for:"
	idx := strings.Index(original, key)
	if idx < 0 {
		return "", fmt.Errorf("line does not contain %q: %q", key, original)
	}
	head := original[:idx+len(key)]
	rest := original[idx+len(key):]

	trimmed := strings.TrimLeft(rest, " \t")
	leadWS := rest[:len(rest)-len(trimmed)]
	if leadWS == "" {
		leadWS = " "
	}

	tail := ""
	if sp := strings.IndexAny(trimmed, " \t"); sp >= 0 {
		tail = trimmed[sp:]
	}

	return head + leadWS + formatDuration(candidate) + tail, nil
}

// fieldIndent derives the indentation for a new field inserted into a rule
// mapping. Given the line of an existing sibling field (fromStartLine
// false), it copies that field's own leading whitespace exactly -- the new
// `for:` then lines up with `expr:`/`labels:`/etc. Given the rule's own
// `- alert:` line instead (fromStartLine true, meaning the rule has no
// other field to copy), it adds two spaces to that line's indentation, the
// conventional one level of nesting under a YAML sequence item's dash.
func fieldIndent(line string, fromStartLine bool) string {
	n := len(line) - len(strings.TrimLeft(line, " "))
	indent := line[:n]
	if fromStartLine {
		return indent + "  "
	}
	return indent
}
