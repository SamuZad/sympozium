// Command celln-scoped-controller runs only the explicitly enabled native
// catalogue controllers in operator-selected namespaces. It has no OCI, channel,
// legacy parent, or standing model-grant dispatcher.
package main

import (
	"flag"
	"fmt"
	api "github.com/sympozium-ai/sympozium/api/v1alpha1"
	"github.com/sympozium-ai/sympozium/internal/cellnscoped"
	"github.com/sympozium-ai/sympozium/internal/controller"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"os"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"strings"
)

func main() {
	namespaces := flag.String("watch-namespaces", "", "required comma-separated isolated namespaces; scope RBAC separately")
	config := flag.String("config", "", "required private operator scoped dispatcher configuration")
	probe := flag.String("health-probe-bind-address", ":8081", "health probe address")
	flag.Parse()
	if err := run(*namespaces, *config, *probe); err != nil {
		fmt.Fprintln(os.Stderr, "scoped controller stopped:", err)
		os.Exit(1)
	}
}

func run(namespaces, config, probe string) error {
	if namespaces == "" || config == "" {
		return fmt.Errorf("explicit namespace isolation and operator configuration required")
	}
	selected := map[string]cache.Config{}
	for _, name := range strings.Split(namespaces, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			return fmt.Errorf("empty namespace is not allowed")
		}
		selected[name] = cache.Config{}
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := api.AddToScheme(scheme); err != nil {
		return err
	}
	ctrl.SetLogger(zap.New())
	manager, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{Scheme: scheme, Cache: cache.Options{DefaultNamespaces: selected}, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: probe})
	if err != nil {
		return err
	}
	dispatcher, err := cellnscoped.LoadDispatcher(config, manager.GetClient(), manager.GetAPIReader())
	if err != nil {
		return err
	}
	runs := &controller.AgentRunReconciler{Client: manager.GetClient(), APIReader: manager.GetAPIReader(), Scheme: scheme, Log: ctrl.Log.WithName("scoped-run"), ScopedOnly: true, ScopedDispatcher: dispatcher}
	if err := runs.SetupWithManager(manager); err != nil {
		return err
	}
	turns := &controller.AgentRunTurnReconciler{Client: manager.GetClient(), APIReader: manager.GetAPIReader(), ScopedDispatcher: dispatcher}
	if err := turns.SetupWithManager(manager); err != nil {
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
