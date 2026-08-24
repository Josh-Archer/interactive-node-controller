package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	availabilityv1alpha1 "github.com/Josh-Archer/interactive-node-controller/api/v1alpha1"
	"github.com/Josh-Archer/interactive-node-controller/internal/controller"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "interactive-node-controller:", err)
		os.Exit(1)
	}
}

func run() error {
	var policy controller.TaintPolicy
	var metricsAddress, probeAddress string
	var leaderElect, showVersion bool
	flag.StringVar(&policy.Key, "taint-key", "availability.interactive-node.io/state", "taint key exclusively managed by this controller")
	flag.StringVar(&policy.InteractiveValue, "interactive-taint-value", "interactive", "taint value used for an interactive desktop")
	flag.StringVar(&policy.ActiveValue, "active-taint-value", "active", "taint value used for an active game")
	flag.StringVar(&policy.FailClosedValue, "fail-closed-taint-value", "unavailable", "taint value used for stale or unknown state")
	flag.DurationVar(&policy.StaleAfter, "stale-after", time.Minute, "heartbeat age after which the node fails closed")
	flag.BoolVar(&policy.FailClosed, "fail-closed", true, "apply NoSchedule for unknown or stale host state")
	flag.StringVar(&metricsAddress, "metrics-bind-address", ":8080", "metrics bind address; use 0 to disable")
	flag.StringVar(&probeAddress, "health-probe-bind-address", ":8081", "health/readiness probe bind address")
	flag.BoolVar(&leaderElect, "leader-elect", true, "enable leader election")
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.Parse()
	if showVersion {
		fmt.Println(version)
		return nil
	}
	if err := policy.Validate(); err != nil {
		return err
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		return err
	}
	if err := availabilityv1alpha1.AddToScheme(scheme); err != nil {
		return err
	}
	manager, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddress},
		HealthProbeBindAddress: probeAddress,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "interactive-node-controller.availability.interactive-node.io",
	})
	if err != nil {
		return err
	}
	reconciler := &controller.NodeActivityReconciler{Client: manager.GetClient(), Policy: policy}
	if err := reconciler.SetupWithManager(manager); err != nil {
		return err
	}
	if err := manager.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err := manager.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}
	return manager.Start(ctrl.SetupSignalHandler())
}
