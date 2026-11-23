// Package controller implements ship-controller: Promotion CRs become ledger MRs,
// and forge state is mirrored back into .status (docs/design.md §12).
package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"juliusbaer.com/codeak/ship/api/v1alpha1"
	"juliusbaer.com/codeak/ship/internal/approval"
	"juliusbaer.com/codeak/ship/internal/config"
	"juliusbaer.com/codeak/ship/internal/driver/crossplane"
	"juliusbaer.com/codeak/ship/internal/forge"
	"juliusbaer.com/codeak/ship/internal/freight"
	"juliusbaer.com/codeak/ship/internal/gitx"
	"juliusbaer.com/codeak/ship/internal/ledger"
	"juliusbaer.com/codeak/ship/internal/policy"
)

type Reconciler struct {
	client.Client
	Forge         forge.Forge // as ship-promoter
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
		log.FromContext(ctx).Info("opened ledger MR", "mr", mr.URL)
	}
	p.Status.LedgerMR = &v1alpha1.MRRef{IID: mr.ID, URL: mr.URL, State: mr.State}
	p.Status.Phase = v1alpha1.PhaseProposed
	p.Status.Message = "ledger MR opened"
	setCond(p, v1alpha1.CondProposed, true, "MROpened", mr.URL)
	setUnknown(p, v1alpha1.CondValidated, "Pending", "waiting for ledger pipeline")
	return r.supersede(ctx, p)
}

func (r *Reconciler) findMR(ctx context.Context, branch string) (*forge.ChangeRequest, error) {
	mrs, err := r.Forge.ListCRs(ctx, r.LedgerProject, forge.CRQuery{State: forge.StateAll, SourceBranch: branch})
	if err != nil || len(mrs) == 0 {
		return nil, err
	}
	return &mrs[0], nil
}

func trailers(p *v1alpha1.Promotion, desc freight.Descriptor) string {
	return fmt.Sprintf("Freight: %s\nRequested-by: %s\nPromotion: %s/%s\nPromotion-UID: %s\n",
		desc.Ref, p.Spec.RequestedBy, p.Namespace, p.Name, p.UID)
}

func (r *Reconciler) openMR(ctx context.Context, p *v1alpha1.Promotion, cfg *config.File, repo gitx.Repo, branch string, desc freight.Descriptor) (*forge.ChangeRequest, error) {
	if _, exists, err := r.Forge.BranchSHA(ctx, r.LedgerProject, branch); err != nil {
		return nil, err
	} else if exists { // leftover from a failed earlier attempt: ours to recreate
		if err := r.Forge.DeleteBranch(ctx, r.LedgerProject, branch); err != nil {
			return nil, err
		}
	}
	path := ledger.Path(cfg.LedgerDir, p.Spec.App, p.Spec.Stage)
	create := true
	if cur, _ := ledger.At(repo, r.Ledger.Branch, cfg.LedgerDir, p.Spec.App, p.Spec.Stage); cur != nil {
		create = false
	}
	title := fmt.Sprintf("Promote %s to %s (%s)", p.Spec.App, p.Spec.Stage, desc.Short())
	body := title + "\n\n"
	if p.Spec.Reason != "" {
		body += p.Spec.Reason + "\n\n"
	}
	body += trailers(p, desc)
	if err := r.Forge.Commit(ctx, r.LedgerProject, forge.CommitSpec{Branch: branch, StartBranch: r.Ledger.Branch, Message: body,
		Files: []forge.FileChange{{Path: path, Create: create, Content: string(ledger.Marshal(ledger.Pin{Freight: desc.Ref}))}}}); err != nil {
		return nil, err
	}
	descr := fmt.Sprintf("Promotion requested via Kubernetes `Promotion` **%s/%s** by **%s**.\n\n", p.Namespace, p.Name, p.Spec.RequestedBy)
	if p.Spec.Reason != "" {
		descr += "> " + p.Spec.Reason + "\n\n"
	}
	descr += "The requester's approval does not count (V8).\n\n```\n" + trailers(p, desc) + "```\n"
	mr, err := r.Forge.OpenCR(ctx, r.LedgerProject, forge.CRSpec{
		SourceBranch: branch, TargetBranch: r.Ledger.Branch, Title: title,
		Description: descr, Labels: []string{"promotion"}, RemoveSourceBranch: true,
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
		mr, err := r.Forge.CR(ctx, r.LedgerProject, o.Status.LedgerMR.IID)
		if err != nil {
			return err
		}
		if mr.State != forge.StateOpen {
			continue // merged ones proceed; closed ones are observed as such
		}
		if err := r.Forge.CloseCR(ctx, r.LedgerProject, mr.ID, fmt.Sprintf("Superseded by Promotion %s/%s (%s).", p.Namespace, p.Name, p.Status.LedgerMR.URL)); err != nil {
			return err
		}
		o.Status.Phase = v1alpha1.PhaseSuperseded
		o.Status.Message = fmt.Sprintf("superseded by %s/%s", p.Namespace, p.Name)
		o.Status.LedgerMR.State = forge.StateClosed
		if err := r.Status().Update(ctx, o); err != nil {
			return err
		}
	}
	return nil
}

// observe mirrors forge state into the Promotion status.
func (r *Reconciler) observe(ctx context.Context, p *v1alpha1.Promotion, cfg *config.File) error {
	mr, err := r.Forge.CR(ctx, r.LedgerProject, p.Status.LedgerMR.IID)
	if err != nil {
		return err
	}
	p.Status.LedgerMR.State = mr.State
	facts, err := approval.Gather(ctx, r.Forge, r.LedgerProject, mr, p.Spec.RequestedBy)
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
	case forge.StateOpen:
		st := ""
		if mr.Pipeline != nil {
			st = mr.Pipeline.Status
		}
		switch st {
		case forge.RunSuccess:
			setCond(p, v1alpha1.CondValidated, true, "PipelinePassed", mr.Pipeline.URL)
			p.Status.Phase = v1alpha1.PhaseAwaitingApproval
			p.Status.Message = "validated; waiting for independent approval and merge"
		case forge.RunFailed, forge.RunCanceled, forge.RunSkipped:
			reject(p, "ledger validation failed", []string{"see " + mr.Pipeline.URL})
			return r.Forge.CloseCR(ctx, r.LedgerProject, mr.ID, "Closed by ship-controller: validation failed, Promotion rejected.")
		default:
			p.Status.Phase = v1alpha1.PhaseProposed
			p.Status.Message = "ledger pipeline " + orDash(st)
		}
	case forge.StateClosed:
		if p.Status.Phase != v1alpha1.PhaseSuperseded {
			p.Status.Phase = v1alpha1.PhaseAbandoned
			p.Status.Message = "ledger MR was closed"
		}
	case forge.StateMerged:
		setCond(p, v1alpha1.CondMerged, true, "Merged", mr.URL)
		if len(mr.MergeSHAs) > 0 {
			p.Status.LedgerCommit = mr.MergeSHAs[0]
		}
		r.observeTargets(ctx, p, cfg)
		pipes, err := r.Forge.Runs(ctx, r.LedgerProject, p.Status.LedgerCommit)
		if err != nil {
			return err
		}
		if len(pipes) == 0 {
			p.Status.Phase, p.Status.Message = v1alpha1.PhaseDeploying, "waiting for deploy pipeline"
			return nil
		}
		pl := pipes[0]
		p.Status.DeployPipeline = &v1alpha1.PipelineRef{ID: pl.ID, URL: pl.URL, Status: pl.Status}
		switch pl.Status {
		case forge.RunSuccess:
			p.Status.Phase, p.Status.Message = v1alpha1.PhaseSucceeded, "deployed"
			setCond(p, v1alpha1.CondDeployed, true, "Deployed", pl.URL)
		case forge.RunFailed, forge.RunCanceled:
			p.Status.Phase, p.Status.Message = v1alpha1.PhaseFailed, "deploy pipeline "+pl.Status+": "+pl.URL
			setCond(p, v1alpha1.CondDeployed, false, "PipelineFailed", pl.URL)
		default:
			p.Status.Phase, p.Status.Message = v1alpha1.PhaseDeploying, "deploy pipeline "+pl.Status
			setUnknown(p, v1alpha1.CondDeployed, "Deploying", pl.URL)
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
	mrs, err := r.Forge.ListCRs(ctx, t.EnvRepo, forge.CRQuery{State: forge.StateAll, SourceBranch: crossplane.Branch(p.Spec.App, p.Spec.Stage, digest)})
	if err != nil || len(mrs) == 0 {
		return
	}
	by, _ := r.Forge.Approvers(ctx, t.EnvRepo, mrs[0].ID)
	p.Status.Targets = []v1alpha1.Target{{Kind: "envMR", URL: mrs[0].URL, State: mrs[0].State, ApprovedBy: by}}
}

func (r *Reconciler) finalize(ctx context.Context, p *v1alpha1.Promotion) error {
	if !controllerutil.ContainsFinalizer(p, v1alpha1.Finalizer) {
		return nil
	}
	if p.Status.LedgerMR != nil {
		mr, err := r.Forge.CR(ctx, r.LedgerProject, p.Status.LedgerMR.IID)
		if err != nil && !forge.IsNotFound(err) {
			return err
		}
		if err == nil && mr.State == forge.StateOpen {
			if err := r.Forge.CloseCR(ctx, r.LedgerProject, mr.ID, fmt.Sprintf("Closed by ship-controller: Promotion %s/%s was deleted.", p.Namespace, p.Name)); err != nil {
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
