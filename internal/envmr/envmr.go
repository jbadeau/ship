// Package envmr implements `ship verify-env-mr`: the provenance check that runs in
// an env repo's MR pipeline and, only if every check passes, approves the MR as
// ship-approver (docs/design.md §7.4). Human MRs are not affected.
package envmr

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"juliusbaer.com/codeak/ship/internal/approval"
	"juliusbaer.com/codeak/ship/internal/config"
	"juliusbaer.com/codeak/ship/internal/driver/crossplane"
	"juliusbaer.com/codeak/ship/internal/forge"
	"juliusbaer.com/codeak/ship/internal/freight"
	"juliusbaer.com/codeak/ship/internal/ledger"
)

type Verifier struct {
	Forge         forge.Forge // authenticated as ship-approver (or read-only with approve=false)
	Reg           freight.Registry
	LedgerProject string
	EnvProject    string
	Log           func(format string, a ...any)
}

type Verdict struct {
	ShipMR   bool
	SHA      string   // head SHA that was verified (and approved, if Approved)
	State    string   // forge.State*; only open CRs are verified
	Checks   []string // passed checks, for the job log
	Failures []string
	Approved bool
	Already  bool // Approved was already true before this run (no new approval)
}

func (v *Verdict) pass(format string, a ...any) {
	v.Checks = append(v.Checks, fmt.Sprintf(format, a...))
}
func (v *Verdict) fail(format string, a ...any) {
	v.Failures = append(v.Failures, fmt.Sprintf(format, a...))
}

// Verify checks env MR iid and approves it if (and only if) it is provably the
// output of a merged, approved ledger promotion.
func (vf Verifier) Verify(ctx context.Context, iid int, approve bool) (Verdict, error) {
	var v Verdict
	gl := vf.Forge
	mr, err := gl.CR(ctx, vf.EnvProject, iid)
	if err != nil {
		return v, err
	}
	if !strings.HasPrefix(mr.SourceBranch, "ship/") {
		return v, nil // not a ship MR: normal approval rules apply
	}
	v.ShipMR, v.SHA, v.State = true, mr.HeadSHA, mr.State
	if mr.State != forge.StateOpen {
		return v, nil // merged/closed: nothing to verify or approve
	}

	// Branch: ship/<app>/<stage>/<digest12>
	parts := strings.Split(mr.SourceBranch, "/")
	if len(parts) != 4 {
		v.fail("source branch %q is not ship/<app>/<stage>/<digest12>", mr.SourceBranch)
		return v, nil
	}
	app, stage, short := parts[1], parts[2], parts[3]

	// Policy comes from the ledger default branch.
	lp, err := gl.Repo(ctx, vf.LedgerProject)
	if err != nil {
		return v, err
	}
	mainSHA, _, err := gl.BranchSHA(ctx, vf.LedgerProject, lp.DefaultBranch)
	if err != nil {
		return v, err
	}
	cfgBytes, found, err := gl.File(ctx, vf.LedgerProject, "ship.yaml", mainSHA)
	if err != nil || !found {
		return v, fmt.Errorf("read ledger ship.yaml: found=%v err=%v", found, err)
	}
	cfg, err := config.Parse(cfgBytes)
	if err != nil {
		return v, err
	}

	// 1. author
	if cfg.Forge.BotUsername == "" {
		v.fail("ship.yaml forge.botUsername is not set")
	} else if mr.Author != cfg.Forge.BotUsername {
		v.fail("author is %q, expected bot %q", mr.Author, cfg.Forge.BotUsername)
	} else {
		v.pass("author is %s", mr.Author)
	}

	// 2. app/stage/driver/target
	a, ok := cfg.Apps[app]
	if !ok || a.Deploy.Driver != "crossplane" {
		v.fail("app %q is not a crossplane app in the ledger", app)
		return v, nil
	}
	if _, ok := cfg.Stage(stage); !ok {
		v.fail("unknown stage %q", stage)
		return v, nil
	}
	tgt, err := crossplane.TargetFromConfig(cfg.DriverConfig(app, stage))
	if err != nil {
		v.fail("driver config: %v", err)
		return v, nil
	}
	if tgt.EnvRepo != vf.EnvProject {
		v.fail("%s/%s deploys to %q, not this repo %q", app, stage, tgt.EnvRepo, vf.EnvProject)
		return v, nil
	}
	ep, err := gl.Repo(ctx, vf.EnvProject)
	if err != nil {
		return v, err
	}
	wantTarget := tgt.TargetBranch
	if wantTarget == "" {
		wantTarget = ep.DefaultBranch
	}
	if mr.TargetBranch != wantTarget {
		v.fail("target branch %q, expected %q", mr.TargetBranch, wantTarget)
	}

	// 3. trailers + ledger state: the MR's Freight must be the *current* ledger pin.
	tr := crossplane.ParseTrailers(mr.Description)
	fr, lcommit := tr["Freight"], tr["Ledger-Commit"]
	lmr, _ := strconv.Atoi(tr["Ledger-MR"])
	pinBytes, found, err := gl.File(ctx, vf.LedgerProject, ledger.Path(cfg.LedgerDir, app, stage), mainSHA)
	if err != nil {
		return v, err
	}
	var pin ledger.Pin
	if found {
		pin, err = ledger.Parse(pinBytes)
	}
	switch {
	case !found || err != nil:
		v.fail("no valid ledger pin for %s/%s on %s", app, stage, lp.DefaultBranch)
	case pin.Freight != fr:
		v.fail("MR Freight %q is not the current ledger pin %q", fr, pin.Freight)
	case freight.ShortDigest(pin.Digest()) != short:
		v.fail("branch digest %s does not match pin %s", short, pin.Digest())
	default:
		v.pass("Freight %s is the current ledger pin of %s/%s", freight.ShortDigest(pin.Digest()), app, stage)
	}
	if lcommit == "" {
		v.fail("missing Ledger-Commit trailer")
	} else if ok, err := gl.IsAncestor(ctx, vf.LedgerProject, lcommit, mainSHA); err != nil || !ok {
		v.fail("ledger commit %s is not merged on %s (%v)", lcommit, lp.DefaultBranch, err)
	} else {
		v.pass("ledger commit %s is on %s", lcommit[:min(12, len(lcommit))], lp.DefaultBranch)
	}

	// 4. ledger MR merged, produced that commit, and was approved by someone other than its author.
	if lmr == 0 {
		v.fail("missing Ledger-MR trailer")
	} else if lm, err := gl.CR(ctx, vf.LedgerProject, lmr); err != nil {
		v.fail("ledger MR !%d: %v", lmr, err)
	} else {
		if lm.State != forge.StateMerged {
			v.fail("ledger MR !%d is %s, not merged", lmr, lm.State)
		}
		if lcommit != "" && !slices.Contains(lm.MergeSHAs, lcommit) {
			v.fail("ledger MR !%d did not produce commit %s", lmr, lcommit)
		}
		facts, err := approval.Gather(ctx, gl, vf.LedgerProject, lm)
		if err != nil {
			return v, err
		}
		if r := approval.Check(cfg, facts); !r.OK() {
			v.fail("ledger MR !%d: %s", lmr, r.Violation)
		} else {
			v.pass("ledger MR !%d merged with independent approval by %s (V8)", lmr, strings.Join(r.Independent, ", "))
		}
	}

	// 5. diff: exactly the claim file, and exactly the re-rendered claim.
	diffs, err := gl.Diffs(ctx, vf.EnvProject, iid)
	if err != nil {
		return v, err
	}
	if len(diffs) != 1 || diffs[0].NewPath != tgt.Path || diffs[0].OldPath != tgt.Path || diffs[0].Created || diffs[0].Deleted || diffs[0].Renamed {
		var paths []string
		for _, d := range diffs {
			paths = append(paths, d.NewPath)
		}
		v.fail("MR must change exactly %s, changes %v", tgt.Path, paths)
	} else if found && pin.Freight == fr {
		baseClaim, ok1, err1 := gl.File(ctx, vf.EnvProject, tgt.Path, mr.BaseSHA)
		headClaim, ok2, err2 := gl.File(ctx, vf.EnvProject, tgt.Path, mr.HeadSHA)
		mf, ok3, err3 := gl.File(ctx, vf.LedgerProject, tgt.Mapping, mainSHA)
		if !ok1 || !ok2 || !ok3 || err1 != nil || err2 != nil || err3 != nil {
			return v, fmt.Errorf("read claim/mapping: %v %v %v", err1, err2, err3)
		}
		desc, err := vf.Reg.Resolve(ctx, pin.Freight)
		if err != nil {
			v.fail("resolve freight: %v", err)
		} else if want, err := crossplane.RenderClaim(baseClaim, mf, app, stage, desc); err != nil {
			v.fail("re-render claim: %v", err)
		} else if !bytes.Equal(want, headClaim) {
			v.fail("claim content differs from what the promotion renders (only mapped fields may change)")
		} else {
			v.pass("claim equals re-rendered claim (only mapped fields changed)")
		}
	}

	if len(v.Failures) > 0 || !approve {
		return v, nil
	}
	by, err := gl.Approvers(ctx, vf.EnvProject, iid)
	if err != nil {
		return v, err
	}
	me, err := gl.Whoami(ctx)
	if err != nil {
		return v, err
	}
	if slices.Contains(by, me) {
		v.Approved, v.Already = true, true
		return v, nil
	}
	if err := gl.Approve(ctx, vf.EnvProject, iid, mr.HeadSHA); err != nil {
		return v, fmt.Errorf("approve: %w", err)
	}
	v.Approved = true
	note := "✅ **ship verify-env-mr** approved this MR as `" + me + "`:\n\n"
	for _, c := range v.Checks {
		note += "- " + c + "\n"
	}
	_ = gl.Comment(ctx, vf.EnvProject, iid, note)
	return v, nil
}
