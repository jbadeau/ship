// Package approval implements V8: a promotion needs enough approvals from people
// who are neither the MR author, nor any requester of the promotion, nor one of
// ship's service accounts. It is enforced pre-merge (controller status check)
// and post-merge (ship deploy, verify-env-mr).
package approval

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"juliusbaer.com/codeak/ship/internal/config"
	"juliusbaer.com/codeak/ship/internal/forge"
	"juliusbaer.com/codeak/ship/internal/trailer"
)

// Facts about one ledger MR.
type Facts struct {
	Author     string   // forge username of the CR author
	Requesters []string // raw requester identities (Requested-by trailers, Promotion spec)
	Approvers  []string // forge usernames that approved
}

type Result struct {
	Independent []string          // approvals that count
	Excluded    map[string]string // approver -> reason it does not count
	Required    int
	Violation   string // empty when V8 holds
}

func (r Result) OK() bool { return r.Violation == "" }

// Check evaluates V8.
func Check(cfg *config.File, f Facts) Result {
	r := Result{Excluded: map[string]string{}, Required: cfg.Forge.LedgerMinApprovals}
	norm := func(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
	requesters := map[string]bool{}
	for _, q := range f.Requesters {
		if q != "" {
			requesters[norm(cfg.ForgeUser(q))] = true
			requesters[norm(q)] = true
		}
	}
	bots := map[string]bool{}
	for _, b := range cfg.Forge.ServiceAccounts() {
		bots[norm(b)] = true
	}
	for _, a := range f.Approvers {
		switch n := norm(a); {
		case n == norm(f.Author):
			r.Excluded[a] = "MR author"
		case requesters[n]:
			r.Excluded[a] = "requester of the promotion"
		case bots[n]:
			r.Excluded[a] = "ship service account"
		default:
			if !slices.Contains(r.Independent, a) {
				r.Independent = append(r.Independent, a)
			}
		}
	}
	if len(r.Independent) < r.Required {
		r.Violation = fmt.Sprintf("V8: %d independent approval(s), %d required", len(r.Independent), r.Required)
		if len(r.Excluded) > 0 {
			var parts []string
			for a, why := range r.Excluded {
				parts = append(parts, a+" ("+why+")")
			}
			slices.Sort(parts)
			r.Violation += "; not counted: " + strings.Join(parts, ", ")
		}
	}
	return r
}

// Gather collects Facts for a ledger CR from the forge. Requesters are the union of
// Requested-by trailers in the description and every commit: extra entries can
// only make V8 stricter. extra adds requesters known from elsewhere (a Promotion CR).
func Gather(ctx context.Context, fg forge.Forge, project string, mr forge.ChangeRequest, extra ...string) (Facts, error) {
	f := Facts{Author: mr.Author, Requesters: append(trailer.All(mr.Description, "Requested-by"), extra...)}
	commits, err := fg.CRCommits(ctx, project, mr.ID)
	if err != nil {
		return f, err
	}
	for _, c := range commits {
		f.Requesters = append(f.Requesters, trailer.All(c.Message, "Requested-by")...)
	}
	f.Approvers, err = fg.Approvers(ctx, project, mr.ID)
	return f, err
}
