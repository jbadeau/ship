package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"juliusbaer.com/codeak/ship/api/v1alpha1"
	"juliusbaer.com/codeak/ship/internal/approval"
	"juliusbaer.com/codeak/ship/internal/forge"
	"juliusbaer.com/codeak/ship/internal/trailer"
)

// StatusChecker is the pre-merge half of V8: it reports a required check on
// ledger CRs (on GitLab: an external status check, Ultimate), reporting "passed" for an MR only when
// it has independent approval. With "status checks must pass" enabled, a
// self-approved promotion cannot merge. It covers every open ledger MR, whether
// it came from a Promotion CR, the CLI, or an agent.
type StatusChecker struct {
	Reader        client.Reader
	Forge         forge.Forge
	LedgerProject string
	Ledger        *LedgerCache
	CheckKey      string // forge.Check.Key: GitLab external status check id
	Interval      time.Duration

	last map[int]string // iid -> "sha:passed|failed" already reported
}

func (s *StatusChecker) Start(ctx context.Context) error {
	t := time.NewTicker(s.Interval)
	defer t.Stop()
	for {
		if err := s.Once(ctx); err != nil {
			log.FromContext(ctx).Error(err, "status check round failed")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Evaluate decides V8 for one MR. For MRs opened from a Promotion CR, the CR must
// still exist with the same UID, and its admission-checked requestedBy is a requester.
func (s *StatusChecker) Evaluate(ctx context.Context, mr forge.ChangeRequest) (bool, string, error) {
	cfg, _, err := s.Ledger.Load(ctx)
	if err != nil {
		return false, "", err
	}
	var extra []string
	if uid := trailer.First(mr.Description, "Promotion-UID"); uid != "" {
		ns, name, _ := strings.Cut(trailer.First(mr.Description, "Promotion"), "/")
		var p v1alpha1.Promotion
		if err := s.Reader.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &p); err != nil {
			return false, fmt.Sprintf("Promotion %s/%s not found: %v", ns, name, err), nil
		}
		if string(p.UID) != uid {
			return false, fmt.Sprintf("Promotion %s/%s UID mismatch", ns, name), nil
		}
		extra = append(extra, p.Spec.RequestedBy)
	}
	facts, err := approval.Gather(ctx, s.Forge, s.LedgerProject, mr, extra...)
	if err != nil {
		return false, "", err
	}
	res := approval.Check(cfg, facts)
	if !res.OK() {
		return false, res.Violation, nil
	}
	return true, "independent approval by " + strings.Join(res.Independent, ", "), nil
}

func (s *StatusChecker) Once(ctx context.Context) error {
	if s.last == nil {
		s.last = map[int]string{}
	}
	mrs, err := s.Forge.ListCRs(ctx, s.LedgerProject, forge.CRQuery{State: forge.StateOpen, TargetBranch: s.Ledger.Branch})
	if err != nil {
		return err
	}
	for _, mr := range mrs {
		ok, why, err := s.Evaluate(ctx, mr)
		if err != nil {
			return err
		}
		key := fmt.Sprintf("%s:%v", mr.HeadSHA, ok)
		if s.last[mr.ID] == key {
			continue
		}
		if err := s.Forge.ReportCheck(ctx, s.LedgerProject, mr.ID, mr.HeadSHA, forge.Check{Key: s.CheckKey, Passed: ok, Summary: why}); err != nil {
			return err
		}
		s.last[mr.ID] = key
		log.FromContext(ctx).Info("status check", "mr", mr.ID, "passed", ok, "reason", why)
	}
	return nil
}
