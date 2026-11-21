// Package promote constructs a promotion: a branch + commit changing one pin,
// optionally pushed with GitLab push options so the MR is created on push.
package promote

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jbadeau/ship/internal/freight"
	"github.com/jbadeau/ship/internal/ledger"
	"github.com/jbadeau/ship/internal/policy"
)

type Options struct {
	App, Stage  string
	Freight     string // full digest ref, or "sha256:…" relative to the app repository
	Reason      string
	RequestedBy string
	Push        bool
	Remote      string // default "origin"
	Target      string // MR target branch, default "main"
}

type Result struct {
	Branch     string
	Freight    freight.Descriptor
	Pushed     bool
	Violations []string // advisory pre-check; validate in CI is authoritative
}

// Run creates the promotion branch and commit. It refuses to proceed if the
// pre-check (same rules as CI validate, judged against HEAD) fails.
func Run(ctx context.Context, c policy.Checker, o Options) (Result, error) {
	if o.Remote == "" {
		o.Remote = "origin"
	}
	if o.Target == "" {
		o.Target = "main"
	}
	app, ok := c.Cfg.Apps[o.App]
	if !ok {
		return Result{}, fmt.Errorf("unknown app %q", o.App)
	}
	ref := o.Freight
	if strings.HasPrefix(ref, "sha256:") {
		ref = app.Repository + "@" + ref
	}
	if st, err := c.Repo.Run("status", "--porcelain"); err != nil || st != "" {
		return Result{}, fmt.Errorf("working tree must be clean (%v)", err)
	}
	orig, err := c.Repo.Run("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return Result{}, err
	}

	pin := ledger.Pin{Freight: ref}
	desc, violations := c.CheckPin(ctx, "HEAD", o.App, o.Stage, pin)
	if len(violations) > 0 {
		return Result{Violations: violations}, fmt.Errorf("promotion would be rejected:\n  %s", strings.Join(violations, "\n  "))
	}
	if cur, _ := ledger.At(c.Repo, "HEAD", c.Cfg.LedgerDir, o.App, o.Stage); cur != nil && cur.Digest() == desc.Digest {
		return Result{}, fmt.Errorf("%s is already pinned in %s/%s", desc.Short(), o.App, o.Stage)
	}

	branch := fmt.Sprintf("promote/%s/%s/%s", o.App, o.Stage, desc.Short())
	if _, err := c.Repo.Run("checkout", "-b", branch); err != nil {
		return Result{}, err
	}
	defer c.Repo.Run("checkout", orig)

	rel := ledger.Path(c.Cfg.LedgerDir, o.App, o.Stage)
	abs := filepath.Join(c.Repo.Dir, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return Result{}, err
	}
	if err := os.WriteFile(abs, ledger.Marshal(pin), 0o644); err != nil {
		return Result{}, err
	}
	title := fmt.Sprintf("Promote %s to %s (%s)", o.App, o.Stage, desc.Short())
	msg := title + "\n\n"
	if o.Reason != "" {
		msg += o.Reason + "\n\n"
	}
	msg += "Freight: " + desc.Ref + "\n"
	if o.RequestedBy != "" {
		msg += "Requested-by: " + o.RequestedBy + "\n"
	}
	if _, err := c.Repo.Run("add", rel); err != nil {
		return Result{}, err
	}
	if _, err := c.Repo.Run("commit", "-m", msg); err != nil {
		return Result{}, err
	}
	res := Result{Branch: branch, Freight: *desc}
	if o.Push {
		desc := strings.ReplaceAll(fmt.Sprintf("Freight: %s · Reason: %s · Requested-by: %s", res.Freight.Ref, o.Reason, o.RequestedBy), "\n", " ")
		if _, err := c.Repo.Run("push",
			"-o", "merge_request.create",
			"-o", "merge_request.target="+o.Target,
			"-o", "merge_request.remove_source_branch",
			"-o", "merge_request.title="+title,
			"-o", "merge_request.description="+desc,
			"-o", "merge_request.label=promotion",
			o.Remote, branch); err != nil {
			return res, err
		}
		res.Pushed = true
	}
	return res, nil
}
