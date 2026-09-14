package pr

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

var slugInvalid = regexp.MustCompile(`[^a-z0-9-]+`)

// slug lowercases s and replaces every run of characters unsafe in a git
// branch name (or merely surprising in one -- spaces, slashes, unicode)
// with a single hyphen.
func slug(s string) string {
	s = strings.ToLower(s)
	s = slugInvalid.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "rule"
	}
	return s
}

// BranchName returns a deterministic, collision-resistant branch name for
// one proposal: noisefloor/<kind>/<group>-<alertname>-<fp>.
//
// Deterministic in (kind, group, alertName, fingerprint) so re-running the
// bot against an UNCHANGED proposal reuses the same branch name -- which is
// what Runner's idempotence check (an existing open PR on this branch)
// depends on. fingerprint is the caller's summary of what would actually
// change (the candidate for: value for a tune, or nothing beyond the
// rule's identity for a retire, which only ever has one possible shape):
// if the underlying proposal changes -- a different candidate for: next
// run -- the branch name changes too, so a new PR is opened rather than
// force-pushing over one a human may already be mid-review on.
//
// Collision-resistant because two different (group, alertName) pairs that
// happen to slug to the same string (e.g. "Demo/A" and "Demo A") still get
// different branch names: the hash is computed over the unslugged inputs,
// not the slug.
func BranchName(kind Kind, group, alertName, fingerprint string) string {
	sum := sha256.Sum256([]byte(string(kind) + "|" + group + "|" + alertName + "|" + fingerprint))
	return fmt.Sprintf("noisefloor/%s/%s-%s-%s",
		kind, slug(group), slug(alertName), hex.EncodeToString(sum[:])[:8])
}
