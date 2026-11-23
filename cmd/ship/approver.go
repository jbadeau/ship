package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"juliusbaer.com/codeak/ship/internal/approver"
	"juliusbaer.com/codeak/ship/internal/secret"
)

// cmdApprover runs the approver service (docs/approver-service.md).
func cmdApprover(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("approver", flag.ContinueOnError)
	listen := fs.String("listen", ":8080", "HTTP listen address (/webhook, /healthz, /metrics)")
	ledgerProject := fs.String("ledger-project", os.Getenv("SHIP_LEDGER_PROJECT"), "ledger repo path")
	groups := fs.String("groups", os.Getenv("SHIP_ENV_GROUPS"), "comma-separated env groups whose ship/* MRs are verified")
	sweep := fs.Duration("sweep", 5*time.Minute, "re-check open ship/* MRs this often (0 = off)")
	if err := parse(fs, args); err != nil {
		return err
	}
	switch {
	case *ledgerProject == "":
		return fmt.Errorf("--ledger-project (SHIP_LEDGER_PROJECT) is required")
	case strings.TrimSpace(*groups) == "":
		return fmt.Errorf("--groups (SHIP_ENV_GROUPS) is required")
	}
	r, err := rt()
	if err != nil {
		return err
	}
	if r.forgeErr != nil {
		return r.forgeErr
	}
	ws, err := r.Secrets.Get(ctx, secret.WebhookSecret)
	if err != nil {
		return err
	}
	if len(ws.Reveal()) < 16 {
		return fmt.Errorf("the webhook-secret secret must be at least 16 characters")
	}
	hook, err := r.Webhook(r.Secrets.Source(secret.WebhookSecret), approver.BranchPrefix)
	if err != nil {
		return err
	}
	gl, err := forgeClient(ctx, secret.ForgeApproverToken)
	if err != nil {
		return err
	}
	me, err := gl.Whoami(ctx)
	if err != nil {
		return fmt.Errorf("approver token: %w", err)
	}
	s := &approver.Server{
		Forge: gl, Hook: hook, Reg: registry(nil), LedgerProject: *ledgerProject,
		Groups: strings.Split(*groups, ","), Sweep: *sweep,
		Log: log.New(os.Stderr, "approver ", log.LstdFlags|log.LUTC),
	}
	srv := &http.Server{Addr: *listen, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go s.Run(ctx)
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	s.Log.Printf("listening on %s as %s on %s, groups %s, ledger %s", *listen, me, gl.Name(), *groups, *ledgerProject)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
