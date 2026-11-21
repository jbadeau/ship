package controller

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/jbadeau/ship/api/v1alpha1"
	"github.com/jbadeau/ship/internal/approval"
	"github.com/jbadeau/ship/internal/gitlab"
	"github.com/jbadeau/ship/internal/trailer"
)

// StatusChecker is the pre-merge half of V8: it serves a GitLab external status
// check (Ultimate) on the ledger project, reporting "passed" for an MR only when
// it has independent approval. With "status checks must pass" enabled, a
// self-approved promotion cannot merge. It covers every open ledger MR, whether
// it came from a Promotion CR, the CLI, or an agent.
type StatusChecker struct {
	Reader        client.Reader
	GitLab        *gitlab.Client
	LedgerProject string
	Ledger        *LedgerCache
	CheckID       int
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
func (s *StatusChecker) Evaluate(ctx context.Context, mr gitlab.MR) (bool, string, error) {
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
	facts, err := approval.Gather(ctx, s.GitLab, s.LedgerProject, mr, extra...)
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
	mrs, err := s.GitLab.ListMRs(ctx, s.LedgerProject, url.Values{"state": {"opened"}, "target_branch": {s.Ledger.Branch}, "per_page": {"100"}})
	if err != nil {
		return err
	}
	for _, mr := range mrs {
		ok, why, err := s.Evaluate(ctx, mr)
		if err != nil {
			return err
		}
		key := fmt.Sprintf("%s:%v", mr.SHA, ok)
		if s.last[mr.IID] == key {
			continue
		}
		if err := s.GitLab.StatusCheckResponse(ctx, s.LedgerProject, mr.IID, mr.SHA, s.CheckID, ok); err != nil {
			return err
		}
		s.last[mr.IID] = key
		log.FromContext(ctx).Info("status check", "mr", mr.IID, "passed", ok, "reason", why)
	}
	return nil
}
