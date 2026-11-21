// Package controller implements ship-controller: Promotion CRs become ledger MRs,
// and GitLab state is mirrored back into .status (docs/design.md §12).
package controller

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/jbadeau/ship/api/v1alpha1"
	"github.com/jbadeau/ship/internal/approval"
	"github.com/jbadeau/ship/internal/config"
	"github.com/jbadeau/ship/internal/driver/crossplane"
	"github.com/jbadeau/ship/internal/freight"
	"github.com/jbadeau/ship/internal/gitlab"
	"github.com/jbadeau/ship/internal/gitx"
	"github.com/jbadeau/ship/internal/ledger"
	"github.com/jbadeau/ship/internal/policy"
)

type Reconciler struct {
	client.Client
	GitLab        *gitlab.Client // as ship-promoter
	Reg           freight.Registry
	LedgerProject string
	Ledger        *LedgerCache
	Poll          time.Duration
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&v1alpha1.Promotion{}).Complete(r)
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var p v1alpha1.Promotion
	if err := r.Get(ctx, req.NamespacedName, &p); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !p.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, &p)
	}
	terminal := p.Status.Phase.Terminal()
	if terminal {
		// Nothing left to close: drop the finalizer so deletion is instant.
		if controllerutil.RemoveFinalizer(&p, v1alpha1.Finalizer) {
			return ctrl.Result{}, r.Update(ctx, &p)
		}
		return ctrl.Result{}, nil
	}
	if controllerutil.AddFinalizer(&p, v1alpha1.Finalizer) {
		if err := r.Update(ctx, &p); err != nil {
			return ctrl.Result{}, err
		}
	}

	before := p.Status.DeepCopy()
	stepErr := r.step(ctx, &p)
	if stepErr != nil {
		p.Status.Message = "error: " + stepErr.Error()
	}
	p.Status.ObservedGeneration = p.Generation
	if !equality.Semantic.DeepEqual(before, &p.Status) {
		if err := r.Status().Update(ctx, &p); err != nil {
			return ctrl.Result{}, err
		}
	}
	if stepErr != nil {
		return ctrl.Result{}, stepErr // backoff
	}
	if p.Status.Phase.Terminal() {
		return ctrl.Result{}, nil // the status update triggers the finalizer cleanup
	}
	return ctrl.Result{RequeueAfter: r.Poll}, nil
}

func (r *Reconciler) step(ctx context.Context, p *v1alpha1.Promotion) error {
	cfg, repo, err := r.Ledger.Load(ctx)
	if err != nil {
		return err
	}
	if p.Status.LedgerMR == nil {
		return r.propose(ctx, p, cfg, repo)
	}
	return r.observe(ctx, p, cfg)
}

func reject(p *v1alpha1.Promotion, msg string, violations []string) {
	p.Status.Phase = v1alpha1.PhaseRejected
	p.Status.Message = msg
	p.Status.Violations = violations
	setCond(p, v1alpha1.CondValidated, false, "Rejected", msg+": "+strings.Join(violations, "; "))
}

func setCond(p *v1alpha1.Promotion, typ string, ok bool, reason, msg string) {
	st := metav1.ConditionFalse
	if ok {
		st = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
		Type: typ, Status: st, Reason: reason, Message: msg, ObservedGeneration: p.Generation,
	})
}

func setUnknown(p *v1alpha1.Promotion, typ, reason, msg string) {
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
		Type: typ, Status: metav1.ConditionUnknown, Reason: reason, Message: msg, ObservedGeneration: p.Generation,
	})
}

// propose runs the V1–V6 pre-check against the ledger default branch and opens the ledger MR.
func (r *Reconciler) propose(ctx context.Context, p *v1alpha1.Promotion, cfg *config.File, repo gitx.Repo) error {
	if p.Status.Phase == "" {
		p.Status.Phase = v1alpha1.PhasePending
	}
	app, ok := cfg.Apps[p.Spec.App]
	if !ok {
		reject(p, "pre-check failed", []string{fmt.Sprintf("V1: unknown app %q", p.Spec.App)})
		return nil
	}
	ref := p.Spec.Freight
	if strings.HasPrefix(ref, "sha256:") {
		ref = app.Repository + "@" + ref
	}
	checker := policy.Checker{Cfg: cfg, Repo: repo, Reg: r.Reg}
	desc, v := checker.CheckPin(ctx, r.Ledger.Branch, p.Spec.App, p.Spec.Stage, ledger.Pin{Freight: ref})
	if len(v) > 0 {
		reject(p, "pre-check failed", v)
		return nil
	}
	if cur, _ := ledger.At(repo, r.Ledger.Branch, cfg.LedgerDir, p.Spec.App, p.Spec.Stage); cur != nil && cur.Digest() == desc.Digest {
		reject(p, "already pinned", []string{fmt.Sprintf("%s is already pinned in %s/%s", desc.Short(), p.Spec.App, p.Spec.Stage)})
		return nil
	}
	p.Status.Freight = desc.Ref
	branch := fmt.Sprintf("promote/%s/%s/%s-%s", p.Spec.App, p.Spec.Stage, desc.Short(), string(p.UID)[:8])
	p.Status.Branch = branch

	mr, err := r.findMR(ctx, branch)
	if err != nil {
		return err
	}
	if mr == nil {
		if mr, err = r.openMR(ctx, p, cfg, repo, branch, *desc); err != nil {
			return err
		}
		log.FromContext(ctx).Info("opened ledger MR", "mr", mr.WebURL)
	}
	p.Status.LedgerMR = &v1alpha1.MRRef{IID: mr.IID, URL: mr.WebURL, State: mr.State}
	p.Status.Phase = v1alpha1.PhaseProposed
	p.Status.Message = "ledger MR opened"
	setCond(p, v1alpha1.CondProposed, true, "MROpened", mr.WebURL)
	setUnknown(p, v1alpha1.CondValidated, "Pending", "waiting for ledger pipeline")
	return r.supersede(ctx, p)
}

func (r *Reconciler) findMR(ctx context.Context, branch string) (*gitlab.MR, error) {
	mrs, err := r.GitLab.ListMRs(ctx, r.LedgerProject, url.Values{"source_branch": {branch}, "state": {"all"}, "order_by": {"created_at"}, "sort": {"desc"}})
	if err != nil || len(mrs) == 0 {
		return nil, err
	}
	return &mrs[0], nil
}

func trailers(p *v1alpha1.Promotion, desc freight.Descriptor) string {
	return fmt.Sprintf("Freight: %s\nRequested-by: %s\nPromotion: %s/%s\nPromotion-UID: %s\n",
		desc.Ref, p.Spec.RequestedBy, p.Namespace, p.Name, p.UID)
}

func (r *Reconciler) openMR(ctx context.Context, p *v1alpha1.Promotion, cfg *config.File, repo gitx.Repo, branch string, desc freight.Descriptor) (*gitlab.MR, error) {
	if _, exists, err := r.GitLab.BranchSHA(ctx, r.LedgerProject, branch); err != nil {
		return nil, err
	} else if exists { // leftover from a failed earlier attempt: ours to recreate
		if err := r.GitLab.DeleteBranch(ctx, r.LedgerProject, branch); err != nil {
			return nil, err
		}
	}
	path := ledger.Path(cfg.LedgerDir, p.Spec.App, p.Spec.Stage)
	action := "create"
	if cur, _ := ledger.At(repo, r.Ledger.Branch, cfg.LedgerDir, p.Spec.App, p.Spec.Stage); cur != nil {
		action = "update"
	}
	title := fmt.Sprintf("Promote %s to %s (%s)", p.Spec.App, p.Spec.Stage, desc.Short())
	body := title + "\n\n"
	if p.Spec.Reason != "" {
		body += p.Spec.Reason + "\n\n"
	}
	body += trailers(p, desc)
	if err := r.GitLab.CreateCommit(ctx, r.LedgerProject, branch, r.Ledger.Branch, body,
		[]gitlab.CommitAction{{Action: action, FilePath: path, Content: string(ledger.Marshal(ledger.Pin{Freight: desc.Ref}))}}); err != nil {
		return nil, err
	}
	descr := fmt.Sprintf("Promotion requested via Kubernetes `Promotion` **%s/%s** by **%s**.\n\n", p.Namespace, p.Name, p.Spec.RequestedBy)
	if p.Spec.Reason != "" {
		descr += "> " + p.Spec.Reason + "\n\n"
	}
	descr += "The requester's approval does not count (V8).\n\n```\n" + trailers(p, desc) + "```\n"
	mr, err := r.GitLab.CreateMR(ctx, r.LedgerProject, map[string]any{
		"source_branch": branch, "target_branch": r.Ledger.Branch, "title": title,
		"description": descr, "labels": "promotion", "remove_source_branch": true,
	})
	return &mr, err
}

// supersede closes still-open ledger MRs of older Promotions for the same app/stage.
func (r *Reconciler) supersede(ctx context.Context, p *v1alpha1.Promotion) error {
	var list v1alpha1.PromotionList
	if err := r.List(ctx, &list); err != nil {
		return err
	}
	for i := range list.Items {
		o := &list.Items[i]
		if o.UID == p.UID || o.Spec.App != p.Spec.App || o.Spec.Stage != p.Spec.Stage ||
			o.Status.Phase.Terminal() || o.Status.LedgerMR == nil || !o.CreationTimestamp.Before(&p.CreationTimestamp) {
			continue
		}
		mr, err := r.GitLab.MR(ctx, r.LedgerProject, o.Status.LedgerMR.IID)
		if err != nil {
			return err
		}
		if mr.State != "opened" {
			continue // merged ones proceed; closed ones are observed as such
		}
		if err := r.GitLab.CloseMR(ctx, r.LedgerProject, mr.IID, fmt.Sprintf("Superseded by Promotion %s/%s (%s).", p.Namespace, p.Name, p.Status.LedgerMR.URL)); err != nil {
			return err
		}
		o.Status.Phase = v1alpha1.PhaseSuperseded
		o.Status.Message = fmt.Sprintf("superseded by %s/%s", p.Namespace, p.Name)
		o.Status.LedgerMR.State = "closed"
		if err := r.Status().Update(ctx, o); err != nil {
			return err
		}
	}
	return nil
}

// observe mirrors GitLab state into the Promotion status.
func (r *Reconciler) observe(ctx context.Context, p *v1alpha1.Promotion, cfg *config.File) error {
	mr, err := r.GitLab.MR(ctx, r.LedgerProject, p.Status.LedgerMR.IID)
	if err != nil {
		return err
	}
	p.Status.LedgerMR.State = mr.State
	facts, err := approval.Gather(ctx, r.GitLab, r.LedgerProject, mr, p.Spec.RequestedBy)
	if err != nil {
		return err
	}
	p.Status.LedgerMR.ApprovedBy = facts.Approvers
	if res := approval.Check(cfg, facts); res.OK() {
		setCond(p, v1alpha1.CondApproved, true, "IndependentApproval", "approved by "+strings.Join(res.Independent, ", "))
	} else {
		setCond(p, v1alpha1.CondApproved, false, "V8", res.Violation)
	}

	switch mr.State {
	case "opened":
		st := ""
		if mr.HeadPipeline != nil {
			st = mr.HeadPipeline.Status
		}
		switch st {
		case "success":
			setCond(p, v1alpha1.CondValidated, true, "PipelinePassed", mr.HeadPipeline.WebURL)
			p.Status.Phase = v1alpha1.PhaseAwaitingApproval
			p.Status.Message = "validated; waiting for independent approval and merge"
		case "failed", "canceled", "skipped":
			reject(p, "ledger validation failed", []string{"see " + mr.HeadPipeline.WebURL})
			return r.GitLab.CloseMR(ctx, r.LedgerProject, mr.IID, "Closed by ship-controller: validation failed, Promotion rejected.")
		default:
			p.Status.Phase = v1alpha1.PhaseProposed
			p.Status.Message = "ledger pipeline " + orDash(st)
		}
	case "closed":
		if p.Status.Phase != v1alpha1.PhaseSuperseded {
			p.Status.Phase = v1alpha1.PhaseAbandoned
			p.Status.Message = "ledger MR was closed"
		}
	case "merged":
		setCond(p, v1alpha1.CondMerged, true, "Merged", mr.WebURL)
		for _, sha := range []string{mr.MergeCommitSHA, mr.SquashCommitSHA, mr.SHA} {
			if sha != "" {
				p.Status.LedgerCommit = sha
				break
			}
		}
		r.observeTargets(ctx, p, cfg)
		pipes, err := r.GitLab.Pipelines(ctx, r.LedgerProject, p.Status.LedgerCommit)
		if err != nil {
			return err
		}
		if len(pipes) == 0 {
			p.Status.Phase, p.Status.Message = v1alpha1.PhaseDeploying, "waiting for deploy pipeline"
			return nil
		}
		pl := pipes[0]
		p.Status.DeployPipeline = &v1alpha1.PipelineRef{ID: pl.ID, URL: pl.WebURL, Status: pl.Status}
		switch pl.Status {
		case "success":
			p.Status.Phase, p.Status.Message = v1alpha1.PhaseSucceeded, "deployed"
			setCond(p, v1alpha1.CondDeployed, true, "Deployed", pl.WebURL)
		case "failed", "canceled":
			p.Status.Phase, p.Status.Message = v1alpha1.PhaseFailed, "deploy pipeline "+pl.Status+": "+pl.WebURL
			setCond(p, v1alpha1.CondDeployed, false, "PipelineFailed", pl.WebURL)
		default:
			p.Status.Phase, p.Status.Message = v1alpha1.PhaseDeploying, "deploy pipeline "+pl.Status
			setUnknown(p, v1alpha1.CondDeployed, "Deploying", pl.WebURL)
		}
	}
	return nil
}

// observeTargets records driver side effects (best effort): crossplane env MRs.
func (r *Reconciler) observeTargets(ctx context.Context, p *v1alpha1.Promotion, cfg *config.File) {
	app := cfg.Apps[p.Spec.App]
	if app.Deploy.Driver != "crossplane" {
		return
	}
	t, err := crossplane.TargetFromConfig(cfg.DriverConfig(p.Spec.App, p.Spec.Stage))
	if err != nil {
		return
	}
	digest := p.Status.Freight[strings.LastIndex(p.Status.Freight, "@")+1:]
	mrs, err := r.GitLab.ListMRs(ctx, t.EnvRepo, url.Values{"source_branch": {crossplane.Branch(p.Spec.App, p.Spec.Stage, digest)}, "state": {"all"}})
	if err != nil || len(mrs) == 0 {
		return
	}
	by, _ := r.GitLab.ApprovedBy(ctx, t.EnvRepo, mrs[0].IID)
	p.Status.Targets = []v1alpha1.Target{{Kind: "envMR", URL: mrs[0].WebURL, State: mrs[0].State, ApprovedBy: by}}
}

func (r *Reconciler) finalize(ctx context.Context, p *v1alpha1.Promotion) error {
	if !controllerutil.ContainsFinalizer(p, v1alpha1.Finalizer) {
		return nil
	}
	if p.Status.LedgerMR != nil {
		mr, err := r.GitLab.MR(ctx, r.LedgerProject, p.Status.LedgerMR.IID)
		if err != nil && !gitlab.IsStatus(err, 404) {
			return err
		}
		if err == nil && mr.State == "opened" {
			if err := r.GitLab.CloseMR(ctx, r.LedgerProject, mr.IID, fmt.Sprintf("Closed by ship-controller: Promotion %s/%s was deleted.", p.Namespace, p.Name)); err != nil {
				return err
			}
		}
	}
	controllerutil.RemoveFinalizer(p, v1alpha1.Finalizer)
	return r.Update(ctx, p)
}

func orDash(s string) string {
	if s == "" {
		return "not started"
	}
	return s
}
