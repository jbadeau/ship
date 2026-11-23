package controller

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"juliusbaer.com/codeak/ship/internal/config"
	"juliusbaer.com/codeak/ship/internal/gitx"
)

// LedgerCache is a bare clone of the ledger repo, refreshed on demand. The
// controller reads policy and pin history from it, so V1–V6 pre-checks reuse the
// exact code that CI runs.
type LedgerCache struct {
	Dir string
	URL string // clone URL, without credentials
	// Auth returns git credentials, resolved on every sync so a rotated token
	// is picked up. nil = no credentials (local paths in tests).
	Auth        func(ctx context.Context) (user, password string, err error)
	Branch      string // default branch, e.g. main
	MinInterval time.Duration

	mu   sync.Mutex
	last time.Time
}

func (l *LedgerCache) Repo() gitx.Repo { return gitx.Repo{Dir: l.Dir} }

func (l *LedgerCache) remote(ctx context.Context) (string, error) {
	if l.Auth == nil {
		return l.URL, nil
	}
	u, err := url.Parse(l.URL)
	if err != nil {
		return "", err
	}
	user, pass, err := l.Auth(ctx)
	if err != nil {
		return "", fmt.Errorf("ledger git credentials: %w", err)
	}
	u.User = url.UserPassword(user, pass)
	return u.String(), nil
}

// redact removes the credentialed remote from git error output.
func redact(err error, remote, plain string) error {
	if err == nil || remote == plain {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), remote, plain))
}

func (l *LedgerCache) Sync(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	remote, err := l.remote(ctx)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(l.Dir, "HEAD")); err != nil {
		if err := os.MkdirAll(filepath.Dir(l.Dir), 0o755); err != nil {
			return err
		}
		if _, err := (gitx.Repo{Dir: filepath.Dir(l.Dir)}).Run("clone", "--bare", "--quiet", remote, l.Dir); err != nil {
			return fmt.Errorf("clone ledger: %w", redact(err, remote, l.URL))
		}
		l.last = time.Now()
		return nil
	}
	if time.Since(l.last) < l.MinInterval {
		return nil
	}
	if _, err := l.Repo().Run("fetch", "--quiet", "--prune", remote, "+refs/heads/*:refs/heads/*"); err != nil {
		return fmt.Errorf("fetch ledger: %w", redact(err, remote, l.URL))
	}
	l.last = time.Now()
	return nil
}

// Load syncs and returns the policy at the head of the default branch.
func (l *LedgerCache) Load(ctx context.Context) (*config.File, gitx.Repo, error) {
	if err := l.Sync(ctx); err != nil {
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
