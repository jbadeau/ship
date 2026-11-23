package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"juliusbaer.com/codeak/ship/api/v1alpha1"
	"juliusbaer.com/codeak/ship/internal/controller"
	"juliusbaer.com/codeak/ship/internal/secret"
)

// cmdController runs ship-controller: Promotion CRs -> ledger MRs, status mirrored
// back, and (with --status-check-id) the pre-merge V8 external status check.
func cmdController(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("controller", flag.ContinueOnError)
	ledgerProject := fs.String("ledger-project", os.Getenv("SHIP_LEDGER_PROJECT"), "ledger repo path")
	checkKey := fs.String("status-check-id", os.Getenv("SHIP_STATUS_CHECK_ID"), "pre-merge V8 check: GitLab external status check id (empty or 0 = disabled; needs Ultimate)")
	poll := fs.Duration("poll", 10*time.Second, "forge polling interval for active Promotions")
	cacheDir := fs.String("cache-dir", filepath.Join(os.TempDir(), "ship-ledger.git"), "bare clone of the ledger repo")
	metrics := fs.String("metrics-bind-address", "0", "metrics address (0 = disabled)")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *ledgerProject == "" {
		return fmt.Errorf("--ledger-project (SHIP_LEDGER_PROJECT) is required")
	}
	gl, err := forgeClient(ctx, secret.ForgeToken) // ship-promoter
	if err != nil {
		return err
	}
	ctrl.SetLogger(zap.New())

	proj, err := gl.Repo(ctx, *ledgerProject)
	if err != nil {
		return err
	}
	cache := &controller.LedgerCache{Dir: *cacheDir, URL: proj.CloneURL, Auth: gl.GitCredentials, Branch: proj.DefaultBranch, MinInterval: 3 * time.Second}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return err
	}
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: *metrics},
	})
	if err != nil {
		return err
	}
	rec := &controller.Reconciler{Client: mgr.GetClient(), Forge: gl, Reg: registry(nil),
		LedgerProject: *ledgerProject, Ledger: cache, Poll: *poll}
	if err := rec.SetupWithManager(mgr); err != nil {
		return err
	}
	if k := strings.TrimSpace(*checkKey); k != "" && k != "0" {
		if err := mgr.Add(&controller.StatusChecker{Reader: mgr.GetClient(), Forge: gl, LedgerProject: *ledgerProject,
			Ledger: cache, CheckKey: k, Interval: *poll}); err != nil {
			return err
		}
	} else {
		fmt.Fprintln(os.Stderr, "V8 pre-merge status check disabled (no --status-check-id); V8 is still enforced at deploy")
	}
	return mgr.Start(ctx)
}
