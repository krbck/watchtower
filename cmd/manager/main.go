// watchtower-manager is the Watchtower operator entrypoint.
//
// Phase 1: watches the ConnectivityCheck CRD, runs the Service→Pod→Port
// chain and writes the outcome into the status. Works out-of-cluster
// (kubeconfig) for development and in-cluster for production.
package main

import (
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	watchtowerv1alpha1 "github.com/krbck/watchtower/api/v1alpha1"
	"github.com/krbck/watchtower/internal/api"
	"github.com/krbck/watchtower/internal/controller"
	"github.com/krbck/watchtower/internal/integrations/istio"
	"github.com/krbck/watchtower/internal/topologymapper"
	"github.com/krbck/watchtower/internal/trace"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(watchtowerv1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var probeAddr string
	var apiAddr string
	var leaderElect bool
	var istioMode string

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8480", "metrics endpoint address")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8481", "health probe address")
	flag.StringVar(&apiAddr, "api-bind-address", ":8482", "REST+WS API listen address (D-008)")
	flag.BoolVar(&leaderElect, "leader-elect", false, "leader election (multi-replica)")
	flag.StringVar(&istioMode, "integration-istio", "auto",
		"Istio integration (D-036/D-037): auto = run when networking.istio.io is detected, true = required (warn when missing), false = off")
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mode, err := istio.ParseMode(istioMode)
	if err != nil {
		setupLog.Error(err, "invalid --integration-istio")
		os.Exit(1)
	}

	cfg := ctrl.GetConfigOrDie()
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "watchtower.krbck.io",
	})
	if err != nil {
		setupLog.Error(err, "failed to start manager")
		os.Exit(1)
	}

	if err := (&controller.ConnectivityCheckReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "failed to setup ConnectivityCheck controller")
		os.Exit(1)
	}

	// Cluster-wide topology mapper (D-015) — feeds the Overview map. Informer
	// event handlers drive debounced rebuilds (D-034).
	mapper := topologymapper.New(mgr.GetClient(), mgr.GetCache())

	// Optional Istio integration (D-036/D-037): its own discovery + dynamic
	// informers, never the shared cache. Problems degrade to log lines.
	if mode == istio.ModeDisabled {
		setupLog.Info("istio integration disabled")
	} else {
		integ, err := newIstioIntegration(mode, cfg)
		if err != nil {
			setupLog.Error(err, "istio integration unavailable; continuing without it")
		} else {
			if err := mgr.Add(integ); err != nil {
				setupLog.Error(err, "failed to add istio integration")
				os.Exit(1)
			}
			mapper.Istio = integ
		}
	}

	if err := mgr.Add(mapper); err != nil {
		setupLog.Error(err, "failed to add topology mapper")
		os.Exit(1)
	}

	// Host trace (D-038): reads the informer cache (types watched by the
	// mapper) and the Istio snapshot; never the API server directly.
	traceSrc := trace.CachedSource{Reader: mgr.GetClient()}
	if mapper.Istio != nil {
		traceSrc.Istio = mapper.Istio.Snapshot
	}

	// REST + WebSocket API for the UI (D-008/D-013).
	if err := mgr.Add(&api.Server{Client: mgr.GetClient(), Addr: apiAddr, Mapper: mapper, Informers: mgr.GetCache(), Trace: traceSrc}); err != nil {
		setupLog.Error(err, "failed to add api server")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "failed to add health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "failed to add ready check")
		os.Exit(1)
	}

	setupLog.Info("starting watchtower-manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager stopped")
		os.Exit(1)
	}
}

// newIstioIntegration builds the integration's private clients.
func newIstioIntegration(mode istio.Mode, cfg *rest.Config) (*istio.Integration, error) {
	disc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	md, err := metadata.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return istio.New(mode, disc, dyn, md), nil
}
