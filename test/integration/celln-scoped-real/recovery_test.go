package main

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"github.com/jackc/pgx/v5/pgxpool"
	api "github.com/sympozium-ai/sympozium/api/v1alpha1"
	"github.com/sympozium-ai/sympozium/internal/cellnauthority"
	cap "github.com/sympozium-ai/sympozium/internal/cellncapability"
	"github.com/sympozium-ai/sympozium/internal/cellnscoped"
	"github.com/sympozium-ai/sympozium/internal/controller"
	"github.com/sympozium-ai/sympozium/internal/modelbudget"
	"github.com/sympozium-ai/sympozium/internal/modelgateway"
	"io"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	if database := os.Getenv("CELLN_REVIEW_RECOVERY_DATABASE"); database != "" {
		pool, err := pgxpool.New(ctx, database)
		if err != nil {
			t.Fatal(err)
		}
		defer pool.Close()
		budgets, err := modelbudget.New(pool)
		if err != nil {
			t.Fatal(err)
		}
		authorities, err := modelgateway.NewPostgresAuthorityStore(pool)
		if err != nil {
			t.Fatal(err)
		}
		private, err := os.ReadFile(cfg.Issuer.PrivateKeyFile)
		if err != nil {
			t.Fatal(err)
		}
		block, _ := pem.Decode(private)
		if block == nil {
			t.Fatal("missing original issuer key")
		}
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		key, ok := parsed.(ed25519.PrivateKey)
		if !ok {
			t.Fatal("invalid original issuer key")
		}
		verifier, err := cap.NewVerifier(issuerName, []cap.VerificationKey{{KeyID: cfg.Issuer.KeyID, PublicKey: key.Public().(ed25519.PublicKey)}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		secret, err := os.ReadFile(cfg.Gateway.TokenFile)
		if err != nil {
			t.Fatal(err)
		}
		gateway, err := modelgateway.New(modelgateway.Config{ClusterID: cfg.ClusterID, RegistrationToken: cap.NewToken(strings.TrimSpace(string(secret))), AuthorityReady: func(context.Context) error { return nil }}, verifier, c, budgets, authorities)
		if err != nil {
			t.Fatal(err)
		}
		server := newTLSServer(gateway.Handler())
		defer server.Close()
		_, public, err := serverTrust(server)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(directory, "gateway-ca.pem")
		if err := os.WriteFile(path, public, 0600); err != nil {
			t.Fatal(err)
		}
		cfg.Gateway.URL, cfg.Gateway.CAFile = server.URL, path
	} else {
		cfg.Gateway = nil
	}
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
	namespace, name := os.Getenv("CELLN_REVIEW_RECOVER_NAMESPACE"), os.Getenv("CELLN_REVIEW_RECOVER_NAME")
	if namespace == "" {
		namespace = "celln-review-a-495"
	}
	if name == "" {
		name = "direct-uppercase"
	}
	if namespace != "celln-review-a-495" && namespace != "celln-review-b-495" {
		t.Fatal("not an isolated review namespace")
	}
	key := types.NamespacedName{Namespace: namespace, Name: name}
	if os.Getenv("CELLN_REVIEW_RECOVER_TURN") == "1" {
		var turn api.AgentRunTurn
		if err := c.Get(ctx, key, &turn); err != nil {
			t.Fatal(err)
		}
		s := turn.Status.CellnScoped
		if s == nil {
			t.Fatal("missing original turn identity")
		}
		prepared, err := d.Store.Load(ctx, s.PreparationName)
		var final *cellnauthority.FinalizedPreparation
		if err == nil {
			final, err = d.Store.LoadFinal(ctx, s.DecisionName, prepared)
			if err != nil {
				t.Fatal(err)
			}
			if string(prepared.UID) != s.PreparationUID || string(final.UID) != s.DecisionUID {
				t.Fatal("protected turn identity changed")
			}
		} else {
			if !apierrors.IsNotFound(err) || s.StartAttempted || len(s.ReceiverID) != 71 || !strings.HasPrefix(s.ReceiverID, "sha256:") {
				t.Fatal("not a recoverable never-started enrollment")
			}
			if _, err := hex.DecodeString(s.ReceiverID[7:]); err != nil {
				t.Fatal("invalid enrolled identity")
			}
			var enrolled struct {
				ID, Owner string
				Decision  cellnauthority.PlatformDecision
			}
			raw, err := os.ReadFile(filepath.Join(root, "scoped/prepared", s.ReceiverID[7:]))
			if err != nil {
				t.Fatal(err)
			}
			if json.Unmarshal(raw, &enrolled) != nil || enrolled.ID != s.ReceiverID || enrolled.Owner != s.Owner {
				t.Fatal("independent enrolled turn mismatch")
			}
			final = &cellnauthority.FinalizedPreparation{Decision: enrolled.Decision}
		}
		if final.Decision.Run.UID != turn.Spec.RunUID || final.Decision.Run.Namespace != turn.Namespace || final.Decision.Parent == nil || final.Decision.Parent.TurnID == nil || *final.Decision.Parent.TurnID != string(turn.UID) {
			t.Fatal("original turn preparation mismatch")
		}
		observed, err := d.Cleanup(ctx, s.ReceiverID, final, s.GatewayRegistrationAttempted)
		if err != nil {
			t.Fatal(err)
		}
		if observed.ID != s.ReceiverID || observed.Owner != s.Owner || !observed.CleanupConfirmed {
			t.Fatal("original turn cleanup unconfirmed")
		}
		s.CleanupConfirmed = true
		s.NativePhase = observed.Phase
		if err := c.Status().Update(ctx, &turn); err != nil {
			t.Fatal(err)
		}
		r := &controller.AgentRunTurnReconciler{Client: c, APIReader: c, ScopedDispatcher: d}
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatal(err)
		}
		t.Log("exact original turn native and gateway fences confirmed; no replacement work submitted")
		return
	}
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
	if final.Decision.Route.Provider != "none" && cfg.Gateway == nil {
		t.Fatal("original model allowance requires the existing recovery database")
	}
	observed, err := d.Cleanup(ctx, s.ReceiverID, final, s.GatewayRegistrationAttempted)
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
