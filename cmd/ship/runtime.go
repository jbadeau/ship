package main

import (
	"context"
	"fmt"
	"sync"

	"juliusbaer.com/codeak/ship/internal/forge"
	"juliusbaer.com/codeak/ship/internal/forge/forges"
	"juliusbaer.com/codeak/ship/internal/secret"
)

// rt is the process runtime: which forge ship talks to, the CI context it runs
// in, and where its secrets come from (docs/adapters.md). It is read once from
// the environment; errors surface only in commands that need it.
var rt = sync.OnceValues(func() (*runtimeEnv, error) {
	fe, ferr := forges.FromEnv(nil)
	sec, serr := secret.NewResolver(nil)
	return &runtimeEnv{Env: fe, Secrets: sec, forgeErr: ferr}, serr
})

type runtimeEnv struct {
	forges.Env
	Secrets  *secret.Resolver
	forgeErr error
}

// ci returns the CI context, or an empty one when not running in the forge's CI.
func ci() forge.CI {
	r, err := rt()
	if err != nil || r.forgeErr != nil {
		return forge.CI{}
	}
	return r.CI
}

// forgeClient returns a forge client authenticated with secret name.
func forgeClient(ctx context.Context, name string) (forge.Forge, error) {
	r, err := rt()
	if err != nil {
		return nil, err
	}
	if r.forgeErr != nil {
		return nil, r.forgeErr
	}
	if _, err := r.Secrets.Get(ctx, name); err != nil {
		ref, _ := r.Secrets.Ref(name)
		return nil, fmt.Errorf("%w (reference %s; override with %s)", err, ref, secret.OverrideVar(name))
	}
	return r.Open(r.Secrets.Source(name))
}

// optionalForge is forgeClient, but nil (no error) when the URL or the secret is
// simply not configured, e.g. `ship plan` outside CI. Other errors still fail.
func optionalForge(ctx context.Context, name string) (forge.Forge, error) {
	r, err := rt()
	if err != nil {
		return nil, err
	}
	if r.forgeErr != nil {
		return nil, r.forgeErr
	}
	if r.URL == "" {
		return nil, nil
	}
	if _, err := r.Secrets.Get(ctx, name); secret.IsNotFound(err) {
		return nil, nil
	}
	return forgeClient(ctx, name)
}

func secrets() (*secret.Resolver, error) {
	r, err := rt()
	if err != nil {
		return nil, err
	}
	return r.Secrets, nil
}
