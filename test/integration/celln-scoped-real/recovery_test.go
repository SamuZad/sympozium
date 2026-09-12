package main

import (
	"context"
	"encoding/json"
	api "github.com/sympozium-ai/sympozium/api/v1alpha1"
	"github.com/sympozium-ai/sympozium/internal/cellnauthority"
	"github.com/sympozium-ai/sympozium/internal/cellnscoped"
	"github.com/sympozium-ai/sympozium/internal/controller"
	"io"
	"k8s.io/apimachinery/pkg/types"
	"log"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	ctrl "sigs.k8s.io/controller-runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// Explicit recovery uses the original issuer, prepared operation, receiver root
// and owner. It cannot issue launch/model authority or replace a lost allowance.
func TestRecoverScopedReview(t *testing.T) {
	base, root, recovery := os.Getenv("CELLN_REVIEW_STATE"), os.Getenv("CELLN_REVIEW_NATIVE_ROOT"), os.Getenv("CELLN_REVIEW_RECOVERY")
	if base == "" || root == "" || recovery == "" {
		t.Skip("explicit retained review state required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	o := options{kubeconfig: filepath.Join(base, "kubeconfig"), contextName: "kubernetes-admin@kubernetes", cellnRoot: root, cellnRuntimeDir: "/tmp/sympozium-celln-tenancy-495", cellnBinary: "/tmp/sympozium-celln-tenancy-495/target/x86_64-unknown-linux-musl/release/celln", cellnTokenFile: filepath.Join(base, "operator/dispatch.token"), scopedOperatorTokenFile: filepath.Join(base, "operator/scoped.token")}
	c, _, scheme, err := liveClient(o)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(recovery, "scoped-controller.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg cellnscoped.Config
	if json.Unmarshal(raw, &cfg) != nil {
		t.Fatal("invalid retained operator config")
	}
	address, err := unusedLoopbackAddress()
	if err != nil {
		t.Fatal(err)
	}
	process, err := startCelln(ctx, o, address, filepath.Join(recovery, "issuer-jwks.json"), cfg.Gateway.URL, filepath.Join(recovery, "gateway-ca.pem"), filepath.Join(recovery, "scoped-parent-template.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer stopProcess(process)
	if err := waitForListener(ctx, address, process); err != nil {
		t.Fatal(err)
	}
	target, _ := url.Parse("http://" + address)
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorLog = log.New(io.Discard, "", 0)
	server := newTLSServer(proxy)
	defer server.Close()
	_, public, err := serverTrust(server)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	ca := filepath.Join(directory, "ca.pem")
	if err := os.WriteFile(ca, public, 0600); err != nil {
		t.Fatal(err)
	}
	cfg.Issuer.Name = issuerName // Contract issuer; the original key and work scope are unchanged.
	cfg.Receiver.URL, cfg.Receiver.CAFile = server.URL, ca
	cfg.Gateway = nil // Recovery below refuses any credential-bearing model route.
	raw, err = json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(directory, "controller.json")
	if err := os.WriteFile(config, raw, 0600); err != nil {
		t.Fatal(err)
	}
	d, err := cellnscoped.LoadDispatcher(config, c, c)
	if err != nil {
		t.Fatal(err)
	}
	key := types.NamespacedName{Namespace: "celln-review-a-495", Name: "direct-uppercase"}
	var run api.AgentRun
	if err := c.Get(ctx, key, &run); err != nil {
		t.Fatal(err)
	}
	s := run.Status.CellnScoped
	if s == nil {
		t.Fatal("no original scoped identity")
	}
	prepared, err := d.Store.Load(ctx, s.PreparationName)
	var final *cellnauthority.FinalizedPreparation
	if err == nil {
		final, err = d.Store.LoadFinal(ctx, s.DecisionName, prepared)
	} else {
		// Only this explicit operator recovery fixture may use independently
		// enrolled durable receiver material after the failed harness removed
		// its protected ConfigMaps. It still cannot mint launch authority.
		var enrolled struct {
			ID       string                          `json:"id"`
			Owner    string                          `json:"owner"`
			Decision cellnauthority.PlatformDecision `json:"decision"`
		}
		if len(s.ReceiverID) != 71 || !strings.HasPrefix(s.ReceiverID, "sha256:") {
			t.Fatal("invalid original identity")
		}
		raw, readErr := os.ReadFile(filepath.Join(root, "scoped/prepared", s.ReceiverID[7:]))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if json.Unmarshal(raw, &enrolled) != nil || enrolled.ID != s.ReceiverID || enrolled.Owner != s.Owner || enrolled.Decision.Run.UID != string(run.UID) || enrolled.Decision.Run.Namespace != run.Namespace {
			t.Fatal("independent enrollment mismatch")
		}
		final = &cellnauthority.FinalizedPreparation{Decision: enrolled.Decision}
		err = nil
	}
	if err != nil {
		t.Fatal(err)
	}
	if final.Decision.Route.Provider != "none" || s.GatewayRegistrationAttempted {
		t.Fatal("recovery fixture requires the original model-free operation")
	}
	observed, err := d.Cleanup(ctx, s.ReceiverID, final, false)
	if err != nil {
		t.Fatal(err)
	}
	if observed.ID != s.ReceiverID || observed.Owner != s.Owner || !observed.CleanupConfirmed {
		t.Fatal("original owner cleanup remains unconfirmed")
	}
	t.Log("original owner authenticated cleanup confirmed; no replacement execution submitted")
	if prepared == nil {
		// Repair only this already-deleting, model-free review object after the
		// exact independently enrolled owner has confirmed cleanup.
		if run.DeletionTimestamp.IsZero() || observed.ReceiptDigest != "" {
			t.Fatal("manual recovery is limited to never-started deletion")
		}
		run.Status.CellnScoped.CleanupConfirmed = true
		if err := c.Status().Update(ctx, &run); err != nil {
			t.Fatal(err)
		}
		var current api.AgentRun
		if err := c.Get(ctx, key, &current); err != nil {
			t.Fatal(err)
		}
		if current.UID != run.UID || !current.Status.CellnScoped.CleanupConfirmed {
			t.Fatal("recovery identity changed")
		}
		current.Finalizers = slices.DeleteFunc(current.Finalizers, func(value string) bool { return value == "sympozium.ai/agentrun-finalizer" })
		if err := c.Update(ctx, &current); err != nil {
			t.Fatal(err)
		}
		return
	}
	r := &controller.AgentRunReconciler{Client: c, APIReader: c, Scheme: scheme, Log: ctrl.Log.WithName("review-recovery"), ScopedOnly: true, ScopedDispatcher: d}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
}
