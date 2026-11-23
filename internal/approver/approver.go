// Package approver implements `ship approver`: a service that receives env-repo
// change-request webhooks, runs the same provenance check as `ship verify-env-mr`,
// and approves ship MRs as ship-approver. The approver token lives only in this
// service, never in CI (docs/approver-service.md).
package approver

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"juliusbaer.com/codeak/ship/internal/envmr"
	"juliusbaer.com/codeak/ship/internal/forge"
	"juliusbaer.com/codeak/ship/internal/freight"
)

// BranchPrefix marks ship's env CRs.
const BranchPrefix = "ship/"

// Key identifies one change request. Repo is a path or forge id (webhooks carry
// ids; the path is looked up through the API, never taken from the payload).
type Key struct {
	Repo string
	CR   int
}

type Server struct {
	Forge         forge.Forge   // authenticated as ship-approver
	Hook          forge.Webhook // verifies and parses webhooks
	Reg           freight.Registry
	LedgerProject string
	Groups        []string      // env groups (full paths); MRs outside them are ignored
	Sweep         time.Duration // re-check open ship/* MRs this often (0 = off)
	Log           *log.Logger

	// verify is the check; defaults to envmr.Verifier. Replaceable in tests.
	verify func(ctx context.Context, project string, iid int) (envmr.Verdict, error)

	once    sync.Once
	queue   chan Key
	mu      sync.Mutex
	pending map[Key]bool
	noted   map[Key]string    // last head SHA a failure note was posted for
	paths   map[string]string // repo id → path cache

	Stats Stats
}

// Stats are exposed on /metrics.
type Stats struct {
	Webhooks, Ignored, Verified, Approved, Rejected, Errors atomic.Int64
}

func (s *Server) init() {
	s.once.Do(func() {
		s.queue = make(chan Key, 1024)
		s.pending, s.noted, s.paths = map[Key]bool{}, map[Key]string{}, map[string]string{}
		if s.Log == nil {
			s.Log = log.Default()
		}
		if s.verify == nil {
			s.verify = func(ctx context.Context, project string, iid int) (envmr.Verdict, error) {
				return envmr.Verifier{Forge: s.Forge, Reg: s.Reg, LedgerProject: s.LedgerProject, EnvProject: project}.
					Verify(ctx, iid, true)
			}
		}
	})
}

// Handler serves POST /webhook, GET /healthz and GET /metrics.
func (s *Server) Handler() http.Handler {
	s.init()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook", s.webhook)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok\n") })
	mux.HandleFunc("GET /metrics", s.metrics)
	return mux
}

func (s *Server) webhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	if err := s.Hook.Verify(r, body); err != nil {
		http.Error(w, "invalid webhook signature", http.StatusUnauthorized)
		return
	}
	s.Stats.Webhooks.Add(1)
	repo, cr, relevant, err := s.Hook.Parse(body)
	if err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	if !relevant {
		s.Stats.Ignored.Add(1)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.Enqueue(Key{repo, cr})
	w.WriteHeader(http.StatusAccepted)
}

// Enqueue schedules a verification, coalescing duplicates.
func (s *Server) Enqueue(k Key) {
	s.init()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending[k] {
		return
	}
	select {
	case s.queue <- k:
		s.pending[k] = true
	default:
		s.Log.Printf("queue full, dropping %v (sweep will retry)", k)
	}
}

// Pending reports whether k is queued (for tests).
func (s *Server) Pending(k Key) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending[k]
}

// Run processes the queue (one MR at a time) and sweeps until ctx is done.
func (s *Server) Run(ctx context.Context) {
	s.init()
	if s.Sweep > 0 {
		go func() {
			t := time.NewTicker(s.Sweep)
			defer t.Stop()
			for {
				s.sweep(ctx)
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
			}
		}()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case k := <-s.queue:
			s.mu.Lock()
			delete(s.pending, k) // events arriving during processing re-queue
			s.mu.Unlock()
			s.Process(ctx, k)
		}
	}
}

// Process verifies one MR and records the outcome.
func (s *Server) Process(ctx context.Context, k Key) {
	s.init()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	path, err := s.repoPath(ctx, k.Repo)
	if err != nil {
		s.Stats.Errors.Add(1)
		s.Log.Printf("repo %s: %v", k.Repo, err)
		return
	}
	if !InGroups(path, s.Groups) {
		s.Stats.Ignored.Add(1)
		s.Log.Printf("%s!%d: not in env groups %v, ignored", path, k.CR, s.Groups)
		return
	}
	v, err := s.verify(ctx, path, k.CR)
	if err != nil {
		s.Stats.Errors.Add(1)
		s.Log.Printf("%s!%d: %v", path, k.CR, err)
		return
	}
	if !v.ShipMR || v.State != forge.StateOpen {
		return
	}
	s.Stats.Verified.Add(1)
	if len(v.Failures) == 0 {
		if v.Approved && !v.Already {
			s.Stats.Approved.Add(1)
			s.Log.Printf("%s!%d @%.12s: approved (%d checks passed)", path, k.CR, v.SHA, len(v.Checks))
		}
		return
	}
	s.Stats.Rejected.Add(1)
	s.Log.Printf("%s!%d @%.12s: NOT approved: %s", path, k.CR, v.SHA, strings.Join(v.Failures, "; "))
	s.mu.Lock()
	already := s.noted[k] == v.SHA
	s.noted[k] = v.SHA
	s.mu.Unlock()
	if !already && s.Forge != nil {
		note := "❌ **ship approver** did not approve this MR (head `" + short(v.SHA) + "`):\n\n"
		for _, f := range v.Failures {
			note += "- " + f + "\n"
		}
		_ = s.Forge.Comment(ctx, path, k.CR, note)
	}
}

func (s *Server) sweep(ctx context.Context) {
	for _, g := range s.Groups {
		mrs, err := s.Forge.GroupCRs(ctx, g, forge.CRQuery{State: forge.StateOpen})
		if err != nil {
			s.Stats.Errors.Add(1)
			s.Log.Printf("sweep %s: %v", g, err)
			continue
		}
		for _, mr := range mrs {
			if strings.HasPrefix(mr.SourceBranch, BranchPrefix) && mr.RepoID != "" {
				s.Enqueue(Key{mr.RepoID, mr.ID})
			}
		}
	}
}

func (s *Server) repoPath(ctx context.Context, id string) (string, error) {
	s.mu.Lock()
	p, ok := s.paths[id]
	s.mu.Unlock()
	if ok {
		return p, nil
	}
	repo, err := s.Forge.Repo(ctx, id)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.paths[id] = repo.Path
	s.mu.Unlock()
	return repo.Path, nil
}

// InGroups reports whether project path lies under one of the groups.
func InGroups(path string, groups []string) bool {
	for _, g := range groups {
		if g = strings.Trim(g, "/"); g != "" && strings.HasPrefix(path, g+"/") {
			return true
		}
	}
	return false
}

func (s *Server) metrics(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	depth := len(s.pending)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	for _, m := range []struct {
		name string
		v    int64
	}{
		{"ship_approver_webhooks_total", s.Stats.Webhooks.Load()},
		{"ship_approver_ignored_total", s.Stats.Ignored.Load()},
		{"ship_approver_verified_total", s.Stats.Verified.Load()},
		{"ship_approver_approved_total", s.Stats.Approved.Load()},
		{"ship_approver_rejected_total", s.Stats.Rejected.Load()},
		{"ship_approver_errors_total", s.Stats.Errors.Load()},
		{"ship_approver_queue_depth", int64(depth)},
	} {
		fmt.Fprintf(w, "%s %d\n", m.name, m.v)
	}
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
