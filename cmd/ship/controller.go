package main

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/jbadeau/ship/api/v1alpha1"
	"github.com/jbadeau/ship/internal/controller"
)

// cmdController runs ship-controller: Promotion CRs -> ledger MRs, status mirrored
// back, and (with --status-check-id) the pre-merge V8 external status check.
func cmdController(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("controller", flag.ContinueOnError)
	ledgerProject := fs.String("ledger-project", os.Getenv("SHIP_LEDGER_PROJECT"), "ledger GitLab project path")
	checkDefault, _ := strconv.Atoi(os.Getenv("SHIP_STATUS_CHECK_ID"))
	checkID := fs.Int("status-check-id", checkDefault, "GitLab external status check id for V8 (0 = disabled; needs Ultimate)")
	poll := fs.Duration("poll", 10*time.Second, "GitLab polling interval for active Promotions")
	cacheDir := fs.String("cache-dir", filepath.Join(os.TempDir(), "ship-ledger.git"), "bare clone of the ledger repo")
	metrics := fs.String("metrics-bind-address", "0", "metrics address (0 = disabled)")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *ledgerProject == "" {
		return fmt.Errorf("--ledger-project (SHIP_LEDGER_PROJECT) is required")
	}
	gl := gitlabClient("SHIP_GITLAB_TOKEN")
	if gl == nil {
		return fmt.Errorf("SHIP_GITLAB_URL and SHIP_GITLAB_TOKEN (ship-promoter) are required")
	}
	ctrl.SetLogger(zap.New())

	proj, err := gl.Project(ctx, *ledgerProject)
	if err != nil {
		return err
	}
	cloneURL, err := url.Parse(proj.HTTPURLToRepo)
	if err != nil {
		return err
	}
	cloneURL.User = url.UserPassword("oauth2", gl.Token)
	cache := &controller.LedgerCache{Dir: *cacheDir, URL: cloneURL.String(), Branch: proj.DefaultBranch, MinInterval: 3 * time.Second}

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
	rec := &controller.Reconciler{Client: mgr.GetClient(), GitLab: gl, Reg: registry(nil),
		LedgerProject: *ledgerProject, Ledger: cache, Poll: *poll}
	if err := rec.SetupWithManager(mgr); err != nil {
		return err
	}
	if *checkID > 0 {
		if err := mgr.Add(&controller.StatusChecker{Reader: mgr.GetClient(), GitLab: gl, LedgerProject: *ledgerProject,
			Ledger: cache, CheckID: *checkID, Interval: *poll}); err != nil {
			return err
		}
	} else {
		fmt.Fprintln(os.Stderr, "V8 pre-merge status check disabled (--status-check-id=0); V8 is still enforced at deploy")
	}
	return mgr.Start(ctx)
}
