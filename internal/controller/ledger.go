package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jbadeau/ship/internal/config"
	"github.com/jbadeau/ship/internal/gitx"
)

// LedgerCache is a bare clone of the ledger repo, refreshed on demand. The
// controller reads policy and pin history from it, so V1–V6 pre-checks reuse the
// exact code that CI runs.
type LedgerCache struct {
	Dir         string
	URL         string // clone URL (may embed credentials)
	Branch      string // default branch, e.g. main
	MinInterval time.Duration

	mu   sync.Mutex
	last time.Time
}

func (l *LedgerCache) Repo() gitx.Repo { return gitx.Repo{Dir: l.Dir} }

func (l *LedgerCache) Sync() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := os.Stat(filepath.Join(l.Dir, "HEAD")); err != nil {
		if err := os.MkdirAll(filepath.Dir(l.Dir), 0o755); err != nil {
			return err
		}
		if _, err := (gitx.Repo{Dir: filepath.Dir(l.Dir)}).Run("clone", "--bare", "--quiet", l.URL, l.Dir); err != nil {
			return fmt.Errorf("clone ledger: %w", err)
		}
		l.last = time.Now()
		return nil
	}
	if time.Since(l.last) < l.MinInterval {
		return nil
	}
	if _, err := l.Repo().Run("fetch", "--quiet", "--prune", l.URL, "+refs/heads/*:refs/heads/*"); err != nil {
		return fmt.Errorf("fetch ledger: %w", err)
	}
	l.last = time.Now()
	return nil
}

// Load syncs and returns the policy at the head of the default branch.
func (l *LedgerCache) Load(_ context.Context) (*config.File, gitx.Repo, error) {
	if err := l.Sync(); err != nil {
		return nil, gitx.Repo{}, err
	}
	repo := l.Repo()
	b, found, err := repo.Show(l.Branch, "ship.yaml")
	if err != nil || !found {
		return nil, repo, fmt.Errorf("ledger ship.yaml at %s: found=%v err=%v", l.Branch, found, err)
	}
	cfg, err := config.Parse(b)
	return cfg, repo, err
}
