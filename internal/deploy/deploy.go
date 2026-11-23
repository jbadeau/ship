// Package deploy hands changed pins to their app's driver (ship plan / ship deploy).
package deploy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"juliusbaer.com/codeak/ship/internal/config"
	"juliusbaer.com/codeak/ship/internal/driver"
	"juliusbaer.com/codeak/ship/internal/freight"
	"juliusbaer.com/codeak/ship/internal/gitx"
	"juliusbaer.com/codeak/ship/internal/ledger"
	"juliusbaer.com/codeak/ship/internal/policy"
)

type Runner struct {
	Cfg       *config.File
	Repo      gitx.Repo
	Reg       freight.Registry
	Deps      driver.Deps
	Driver    func(name string, deps driver.Deps) (driver.Driver, error)
	Promotion driver.Promotion
}

type Mode struct {
	PlanOnly bool
	Wait     time.Duration // 0 = do not wait
	Record   bool          // push "deployed" evidence after success
}

type Outcome struct {
	App, Stage string
	Freight    string
	Plan       *driver.Plan
	Result     *driver.Result
	Health     *driver.Health
	Err        error
}

// gitFiles serves ledger-repo files at a fixed revision to drivers.
type gitFiles struct {
	repo gitx.Repo
	rev  string
}

func (g gitFiles) ReadFile(p string) ([]byte, error) {
	b, found, err := g.repo.Show(g.rev, p)
	if err == nil && !found {
		err = fmt.Errorf("%s not found at %s", p, g.rev)
	}
	return b, err
}

// Run plans or deploys every pin changed between base and head.
func (r Runner) Run(ctx context.Context, base, head string, m Mode) ([]Outcome, error) {
	pins, _, err := ledger.Changes(r.Repo, base, head, r.Cfg.LedgerDir)
	if err != nil {
		return nil, err
	}
	var outs []Outcome
	var failed bool
	for _, ch := range pins {
		if ch.App == "" || ch.Status == "D" {
			continue // validate rejects these; nothing to deploy
		}
		o := r.one(ctx, base, head, ch, m)
		if o.Err != nil {
			failed = true
		}
		outs = append(outs, o)
	}
	if failed {
		return outs, errors.New("one or more deployments failed")
	}
	return outs, nil
}

func (r Runner) one(ctx context.Context, base, head string, ch ledger.Change, m Mode) (o Outcome) {
	o.App, o.Stage = ch.App, ch.Stage
	app, ok := r.Cfg.Apps[ch.App]
	if !ok {
		o.Err = fmt.Errorf("unknown app %q", ch.App)
		return
	}
	pin, err := ledger.At(r.Repo, head, r.Cfg.LedgerDir, ch.App, ch.Stage)
	if err != nil || pin == nil {
		o.Err = fmt.Errorf("read pin: %v", err)
		return
	}
	o.Freight = pin.Freight
	desc, err := r.Reg.Resolve(ctx, pin.Freight)
	if err != nil {
		o.Err = err
		return
	}
	req := driver.Request{App: ch.App, Stage: ch.Stage, Freight: desc, Config: r.Cfg.DriverConfig(ch.App, ch.Stage),
		Promotion: r.Promotion, Files: gitFiles{r.Repo, head}}
	if r.Promotion.LedgerCommit == "" {
		req.Promotion.LedgerCommit, _ = r.Repo.Resolve(head)
	}
	if prev, _ := ledger.At(r.Repo, base, r.Cfg.LedgerDir, ch.App, ch.Stage); prev != nil {
		if pd, err := r.Reg.Resolve(ctx, prev.Freight); err == nil {
			req.Previous = &pd
		}
	}
	drv, err := r.Driver(app.Deploy.Driver, r.Deps)
	if err != nil {
		o.Err = err
		return
	}
	if m.PlanOnly {
		p, err := drv.Plan(ctx, req)
		o.Plan, o.Err = &p, err
		return
	}
	res, err := drv.Apply(ctx, req)
	if err != nil {
		o.Err = fmt.Errorf("apply: %w", err)
		return
	}
	o.Result = &res
	if m.Wait > 0 {
		wctx, cancel := context.WithTimeout(ctx, m.Wait)
		defer cancel()
		h, err := drv.Wait(wctx, req)
		switch {
		case errors.Is(err, driver.ErrNotSupported):
			h = driver.Health{Ready: true, Message: "driver cannot observe health; applied only"}
		case err != nil:
			o.Err = fmt.Errorf("wait: %w", err)
			return
		case !h.Ready:
			o.Err = fmt.Errorf("wait: not ready: %s", h.Message)
			return
		}
		o.Health = &h
	}
	if m.Record {
		ann := policy.DeployedEvidence(ch.Stage)
		if r.Promotion.PipelineURL != "" {
			ann["ship.dev/pipeline"] = r.Promotion.PipelineURL
		}
		if _, err := r.Reg.PushEvidence(ctx, desc.Ref, freight.EvidenceArtifactType, ann, nil); err != nil {
			o.Err = fmt.Errorf("record deployed evidence: %w", err)
		}
	}
	return
}
