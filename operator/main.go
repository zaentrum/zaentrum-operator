// Command zaentrum-operator runs the controller-runtime manager that reconciles
// the Zaentrum platform from a single Zaentrum CR.
package main

import (
	"flag"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
	"github.com/zaentrum/zaentrum-operator/operator/internal/controller"
	"github.com/zaentrum/zaentrum-operator/operator/internal/digest"
	"github.com/zaentrum/zaentrum-operator/operator/internal/updates"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(zaentrumv1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr, probeAddr string
	var enableLeaderElection bool
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. Ensures only one active manager.")
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Leader-election timings, deliberately far longer than the library
	// defaults (15s lease / 10s renew / 2s retry).
	//
	// Losing the lease makes controller-runtime EXIT the process — correct for
	// a multi-replica manager, where another replica takes over immediately.
	// This operator runs a single replica, so the only thing a lost lease
	// produces is a restart and a gap in reconciliation. With the defaults, an
	// API server that is unreachable for eleven seconds is enough: that is what
	// killed this operator 23 times in 32 days on a small cluster whose control
	// plane blips during etcd compaction.
	//
	// Stretching the deadline to 50s rides out those blips. The cost is bounded
	// and paid only in the case this operator does not have: with two replicas,
	// a takeover after a hard pod kill would wait out the 60s lease. Releasing
	// on cancel keeps graceful shutdowns (a rollout) instant regardless.
	leaseDuration := 60 * time.Second
	renewDeadline := 50 * time.Second
	retryPeriod := 10 * time.Second

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                        scheme,
		Metrics:                       metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress:        probeAddr,
		LeaderElection:                enableLeaderElection,
		LeaderElectionID:              "zaentrum-operator.zaentrum.io",
		LeaseDuration:                 &leaseDuration,
		RenewDeadline:                 &renewDeadline,
		RetryPeriod:                   &retryPeriod,
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// RELEASES_URL points the Stage-2 auto-update logic at the published
	// channel document; empty falls back to the canonical raw GitHub URL.
	releasesURL := os.Getenv("RELEASES_URL")
	if releasesURL == "" {
		releasesURL = updates.DefaultReleasesURL
	}

	// PIN_DIGESTS makes a moving tag (:latest) roll on a new push: each
	// ghcr.io/zaentrum/* image is resolved to its current digest before apply,
	// so a fresh image changes the rendered spec instead of being a no-op. On by
	// default; set PIN_DIGESTS=false to keep bare tags.
	pinDigests := os.Getenv("PIN_DIGESTS") != "false"

	if err := (&controller.ZaentrumReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		ReleasesURL: releasesURL,
		PinDigests:  pinDigests,
		Digest:      digest.New(nil),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Zaentrum")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting zaentrum-operator")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
