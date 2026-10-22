// Command operator runs the Agent Substrate operator, which installs the
// embedded Substrate release as a SubstrateInstall describes.
package main

import (
	"flag"
	"os"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/samueltorres/agent-substrate-operator/api/v1alpha1"
	"github.com/samueltorres/agent-substrate-operator/internal/controller"
	"github.com/samueltorres/agent-substrate-operator/internal/render"
	"github.com/samueltorres/agent-substrate-operator/internal/upstream"
)

func main() {
	var metricsAddr, probeAddr string
	var leaderElect bool
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address the metrics endpoint binds to; 0 disables it.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address the health probe endpoint binds to.")
	flag.BoolVar(&leaderElect, "leader-elect", true, "Elect a leader, so only one replica reconciles.")
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("setup")

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		log.Error(err, "registering client-go types")
		os.Exit(1)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		log.Error(err, "registering SubstrateInstall")
		os.Exit(1)
	}

	// The cache holds only the workloads the operator installed, not every
	// Deployment in the cluster.
	installed := cache.ByObject{Label: labels.SelectorFromSet(labels.Set{render.LabelInstall: "substrate"})}
	cfg := ctrl.GetConfigOrDie()
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "agent-substrate-operator.substrate.samueltorres.github.io",
		Cache: cache.Options{ByObject: map[client.Object]cache.ByObject{
			&appsv1.Deployment{}:  installed,
			&appsv1.DaemonSet{}:   installed,
			&appsv1.StatefulSet{}: installed,
		}},
	})
	if err != nil {
		log.Error(err, "creating the manager")
		os.Exit(1)
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Error(err, "creating the clientset")
		os.Exit(1)
	}
	r := &controller.Reconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Clientset: clientset,
		Exec:      &controller.APIServerExecutor{Config: cfg, Clientset: clientset},
	}
	if err := r.SetupWithManager(mgr); err != nil {
		log.Error(err, "setting up the controller")
		os.Exit(1)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		log.Error(err, "adding the health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		log.Error(err, "adding the readiness check")
		os.Exit(1)
	}

	log.Info("starting", "substrateVersion", upstream.Version)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "running the manager")
		os.Exit(1)
	}
}
