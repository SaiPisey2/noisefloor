package pr

import (
	"fmt"
	"io"
)

// WriteResult prints res to w in the form a human reviews: one block per
// proposal (its branch, the diff, and the full PR body -- everything the
// PR would carry), then what happened to it (opened, already open, or
// dry-run), then every refusal. This is deliberately the same rendering
// whether or not opt.Apply was set, so a dry run shows EXACTLY what -apply
// would have produced.
func WriteResult(w io.Writer, res RunResult, apply bool) {
	opened := map[string]*PullRequest{}
	for _, o := range res.Opened {
		opened[o.Proposal.Branch] = o.PR
	}
	skipped := map[string]*PullRequest{}
	for _, s := range res.Skipped {
		skipped[s.Proposal.Branch] = s.PR
	}

	for i, p := range res.Proposals {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "================================================================\n")
		fmt.Fprintf(w, "%s: %s/%s\n", p.Kind, p.Group, p.AlertName)
		fmt.Fprintf(w, "branch: %s\n", p.Branch)
		fmt.Fprintf(w, "file:   %s\n", p.File)
		fmt.Fprintf(w, "----------------------------------------------------------------\n")
		fmt.Fprintf(w, "diff:\n%s", p.Diff)
		fmt.Fprintf(w, "----------------------------------------------------------------\n")
		fmt.Fprintf(w, "PR title: %s\n", p.Title)
		fmt.Fprintf(w, "PR body:\n%s", p.Body)
		fmt.Fprintf(w, "----------------------------------------------------------------\n")

		switch {
		case opened[p.Branch] != nil:
			fmt.Fprintf(w, "opened: %s\n", opened[p.Branch].URL)
		case skipped[p.Branch] != nil:
			fmt.Fprintf(w, "already open: %s (no new PR opened)\n", skipped[p.Branch].URL)
		case apply:
			fmt.Fprintf(w, "not opened (see error above)\n")
		default:
			fmt.Fprintf(w, "dry run: not opened. Re-run with -apply to open this PR.\n")
		}
	}

	if len(res.Refusals) > 0 {
		if len(res.Proposals) > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "refused (no PR proposed):\n")
		for _, r := range res.Refusals {
			fmt.Fprintf(w, "  - %s\n", r.String())
		}
	}

	if len(res.Proposals) == 0 && len(res.Refusals) == 0 {
		fmt.Fprintln(w, "nothing to propose: no rule crosses a retire or tune verdict this run.")
	}
}
