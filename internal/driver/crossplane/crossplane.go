// Package crossplane implements the crossplane driver: it writes the promoted
// Freight into a Crossplane claim in an env repo by opening an MR there
// (docs/design.md §7.4). Flux syncs the env repo; ship never writes to clusters.
package crossplane

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"juliusbaer.com/codeak/ship/internal/config"
	"juliusbaer.com/codeak/ship/internal/driver"
	"juliusbaer.com/codeak/ship/internal/forge"
	"juliusbaer.com/codeak/ship/internal/freight"
	"juliusbaer.com/codeak/ship/internal/mapping"
	"juliusbaer.com/codeak/ship/internal/textdiff"
)

const Label = "ship"

type Driver struct {
	Deps driver.Deps
	// PollInterval for Wait and auto-merge retries (default 5s).
	PollInterval time.Duration
}

// Target is the resolved driver config for one app/stage.
type Target struct {
	EnvRepo      string // GitLab project path
	Path         string // claim file in the env repo
	Mapping      string // mapping file in the ledger repo
	TargetBranch string // default: env repo default branch
}

func TargetFromConfig(cfg map[string]any) (Target, error) {
	var t Target
	var err error
	if t.EnvRepo, err = config.String(cfg, "envRepo"); err != nil {
		return t, err
	}
	if t.Path, err = config.String(cfg, "path"); err != nil {
		return t, err
	}
	if t.Mapping, err = config.String(cfg, "mapping"); err != nil {
		return t, err
	}
	t.TargetBranch = config.StringOr(cfg, "targetBranch", "")
	return t, nil
}

// Branch is the deterministic env-repo branch for a promotion.
func Branch(app, stage, digest string) string {
	return fmt.Sprintf("ship/%s/%s/%s", app, stage, freight.ShortDigest(digest))
}

// RenderClaim applies the mapping to the current claim. Shared with verify-env-mr,
// which re-renders to prove an env MR is exactly what a promotion produces.
func RenderClaim(claim, mappingFile []byte, app, stage string, d freight.Descriptor) ([]byte, error) {
	m, err := mapping.Parse(mappingFile)
	if err != nil {
		return nil, err
	}
	var data mapping.Data
	data.App, data.Stage = app, stage
	data.Freight.Ref, data.Freight.Repository, data.Freight.Digest = d.Ref, d.Repository, d.Digest
	data.Freight.Short, data.Freight.ArtifactType, data.Freight.Annotations = d.Short(), d.ArtifactType, d.Annotations
	if data.Freight.Annotations == nil {
		data.Freight.Annotations = map[string]string{}
	}
	return mapping.Render(claim, m, data)
}

type state struct {
	t        Target
	branch   string
	current  []byte
	rendered []byte
}

func (d Driver) gl() (forge.Forge, error) {
	if d.Deps.Forge == nil {
		return nil, fmt.Errorf("crossplane driver: forge client not configured (SHIP_FORGE_URL / forge-token secret)")
	}
	return d.Deps.Forge, nil
}

func (d Driver) load(ctx context.Context, r driver.Request) (state, error) {
	gl, err := d.gl()
	if err != nil {
		return state{}, err
	}
	t, err := TargetFromConfig(r.Config)
	if err != nil {
		return state{}, err
	}
	if t.TargetBranch == "" {
		p, err := gl.Repo(ctx, t.EnvRepo)
		if err != nil {
			return state{}, err
		}
		t.TargetBranch = p.DefaultBranch
	}
	cur, found, err := gl.File(ctx, t.EnvRepo, t.Path, t.TargetBranch)
	if err != nil {
		return state{}, err
	}
	if !found {
		return state{}, fmt.Errorf("claim %s not found in %s@%s (claims are created by humans; ship only updates mapped fields)", t.Path, t.EnvRepo, t.TargetBranch)
	}
	mf, err := r.Files.ReadFile(t.Mapping)
	if err != nil {
		return state{}, fmt.Errorf("mapping %s: %w", t.Mapping, err)
	}
	out, err := RenderClaim(cur, mf, r.App, r.Stage, r.Freight)
	if err != nil {
		return state{}, err
	}
	return state{t: t, branch: Branch(r.App, r.Stage, r.Freight.Digest), current: cur, rendered: out}, nil
}

func (d Driver) Plan(ctx context.Context, r driver.Request) (driver.Plan, error) {
	s, err := d.load(ctx, r)
	if err != nil {
		return driver.Plan{}, err
	}
	if bytes.Equal(s.current, s.rendered) {
		return driver.Plan{NoOp: true, Summary: fmt.Sprintf("%s:%s already at %s", s.t.EnvRepo, s.t.Path, r.Freight.Short())}, nil
	}
	return driver.Plan{
		Summary: fmt.Sprintf("open MR in %s (%s -> %s) updating %s", s.t.EnvRepo, s.branch, s.t.TargetBranch, s.t.Path),
		Diff:    textdiff.Lines(string(s.current), string(s.rendered)),
	}, nil
}

func (d Driver) Apply(ctx context.Context, r driver.Request) (driver.Result, error) {
	s, err := d.load(ctx, r)
	if err != nil {
		return driver.Result{}, err
	}
	gl := d.Deps.Forge
	if bytes.Equal(s.current, s.rendered) {
		return driver.Result{Summary: "claim already up to date"}, nil
	}
	open, err := gl.ListCRs(ctx, s.t.EnvRepo, forge.CRQuery{State: forge.StateOpen, Label: Label})
	if err != nil {
		return driver.Result{}, err
	}
	var mine *forge.ChangeRequest
	prefix := fmt.Sprintf("ship/%s/%s/", r.App, r.Stage)
	for i := range open {
		mr := open[i]
		switch {
		case mr.SourceBranch == s.branch:
			mine = &open[i]
		case strings.HasPrefix(mr.SourceBranch, prefix):
			d.Deps.Logf("closing superseded env MR !%d (%s)", mr.ID, mr.SourceBranch)
			if err := gl.CloseCR(ctx, s.t.EnvRepo, mr.ID, "Superseded by promotion of "+r.Freight.Ref); err != nil {
				return driver.Result{}, err
			}
		}
	}
	if mine == nil {
		if _, exists, err := gl.BranchSHA(ctx, s.t.EnvRepo, s.branch); err != nil {
			return driver.Result{}, err
		} else if exists {
			// Leftover from a closed MR: our namespace, safe to recreate.
			if err := gl.DeleteBranch(ctx, s.t.EnvRepo, s.branch); err != nil {
				return driver.Result{}, err
			}
		}
		msg := fmt.Sprintf("ship: %s to %s (%s)\n\n%s", r.App, r.Stage, r.Freight.Short(), trailers(r))
		if err := gl.Commit(ctx, s.t.EnvRepo, forge.CommitSpec{Branch: s.branch, StartBranch: s.t.TargetBranch, Message: msg,
			Files: []forge.FileChange{{Path: s.t.Path, Content: string(s.rendered)}}}); err != nil {
			return driver.Result{}, err
		}
		mr, err := gl.OpenCR(ctx, s.t.EnvRepo, forge.CRSpec{
			SourceBranch: s.branch, TargetBranch: s.t.TargetBranch,
			Title:       fmt.Sprintf("ship: promote %s to %s (%s)", r.App, r.Stage, r.Freight.Short()),
			Description: description(r),
			Labels:      []string{Label}, RemoveSourceBranch: true,
		})
		if err != nil {
			return driver.Result{}, err
		}
		mine = &mr
		d.Deps.Logf("opened env MR %s", mr.URL)
	} else {
		d.Deps.Logf("reusing env MR %s", mine.URL)
	}
	if err := d.autoMerge(ctx, s.t.EnvRepo, mine.ID); err != nil {
		return driver.Result{}, err
	}
	return driver.Result{Changed: true, Summary: "env MR " + mine.URL, Links: []string{mine.URL}}, nil
}

// autoMerge sets merge-when-pipeline-succeeds, retrying while GitLab is still
// creating the MR pipeline or computing mergeability.
func (d Driver) autoMerge(ctx context.Context, project string, iid int) error {
	gl := d.Deps.Forge
	deadline := time.Now().Add(3 * time.Minute)
	for {
		mr, err := gl.CR(ctx, project, iid)
		if err != nil {
			return err
		}
		if mr.State == forge.StateMerged || mr.AutoMerge {
			return nil
		}
		if mr.Pipeline != nil {
			err = gl.AutoMerge(ctx, project, iid, mr.HeadSHA)
			if err == nil {
				return nil
			}
			d.Deps.Logf("auto-merge not accepted yet: %v", err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("could not enable auto-merge on %s!%d: %v", project, iid, err)
		}
		if err := sleep(ctx, d.poll()); err != nil {
			return err
		}
	}
}

func (d Driver) poll() time.Duration {
	if d.PollInterval > 0 {
		return d.PollInterval
	}
	return 5 * time.Second
}

// Wait: phase 1 = env MR merged (or claim already up to date). Phase 2 (cluster
// readiness) needs a read-only kubeconfig and is not implemented in the MVP.
func (d Driver) Wait(ctx context.Context, r driver.Request) (driver.Health, error) {
	gl, err := d.gl()
	if err != nil {
		return driver.Health{}, err
	}
	for {
		s, err := d.load(ctx, r)
		if err != nil {
			return driver.Health{}, err
		}
		if bytes.Equal(s.current, s.rendered) {
			return driver.Health{Ready: true, Message: fmt.Sprintf("%s@%s has %s", s.t.EnvRepo, s.t.TargetBranch, r.Freight.Short())}, nil
		}
		mrs, err := gl.ListCRs(ctx, s.t.EnvRepo, forge.CRQuery{State: forge.StateAll, SourceBranch: s.branch})
		if err != nil {
			return driver.Health{}, err
		}
		if len(mrs) == 0 {
			return driver.Health{}, fmt.Errorf("no env MR for branch %s", s.branch)
		}
		mr, err := gl.CR(ctx, s.t.EnvRepo, mrs[0].ID)
		if err != nil {
			return driver.Health{}, err
		}
		switch {
		case mr.State == forge.StateClosed:
			return driver.Health{}, fmt.Errorf("env MR %s was closed", mr.URL)
		case mr.HasConflicts:
			return driver.Health{}, fmt.Errorf("env MR %s has conflicts", mr.URL)
		case mr.Pipeline != nil && (mr.Pipeline.Status == forge.RunFailed || mr.Pipeline.Status == forge.RunCanceled):
			return driver.Health{}, fmt.Errorf("env MR %s pipeline %s: %s", mr.URL, mr.Pipeline.Status, mr.Pipeline.URL)
		}
		d.Deps.Logf("waiting for env MR %s (state=%s)", mr.URL, mr.State)
		if err := sleep(ctx, d.poll()); err != nil {
			return driver.Health{}, fmt.Errorf("env MR %s not merged: %w", mr.URL, err)
		}
	}
}

func trailers(r driver.Request) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Freight: %s\n", r.Freight.Ref)
	if r.Promotion.LedgerProject != "" {
		fmt.Fprintf(&b, "Ledger-Project: %s\n", r.Promotion.LedgerProject)
	}
	if r.Promotion.LedgerCommit != "" {
		fmt.Fprintf(&b, "Ledger-Commit: %s\n", r.Promotion.LedgerCommit)
	}
	if r.Promotion.LedgerMR != 0 {
		fmt.Fprintf(&b, "Ledger-MR: %d\n", r.Promotion.LedgerMR)
	}
	if r.Promotion.PipelineURL != "" {
		fmt.Fprintf(&b, "Pipeline: %s\n", r.Promotion.PipelineURL)
	}
	return b.String()
}

func description(r driver.Request) string {
	head := fmt.Sprintf("Automated promotion of **%s** to **%s** by ship.\n\n", r.App, r.Stage)
	if r.Promotion.LedgerMRURL != "" {
		head += "Approved in ledger MR: " + r.Promotion.LedgerMRURL + "\n\n"
	}
	return head + "```\n" + trailers(r) + "```\n"
}

// ParseTrailers extracts "Key: value" lines from an MR description or commit message.
func ParseTrailers(text string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ": ")
		if ok && k != "" && !strings.ContainsAny(k, " *`") {
			if _, dup := out[k]; !dup {
				out[k] = strings.TrimSpace(v)
			}
		}
	}
	return out
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
