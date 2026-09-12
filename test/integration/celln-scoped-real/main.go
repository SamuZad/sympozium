// celln-scoped-real is an explicitly invoked live integration harness. It
// drives one caller-owned AgentRun through the production reconciler, native
// Celln scoped receiver, TLS model gateway, and PostgreSQL accounting ledger.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	api "github.com/sympozium-ai/sympozium/api/v1alpha1"
	"github.com/sympozium-ai/sympozium/internal/cellncapability"
	"github.com/sympozium-ai/sympozium/internal/cellnscoped"
	"github.com/sympozium-ai/sympozium/internal/controller"
	"github.com/sympozium-ai/sympozium/internal/modelgateway"
	"github.com/zeebo/blake3"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	inputVersion = "sympozium.ai/celln-scoped-live-input-v1"
	issuerName   = "sympozium-celln-scoped-live"
	issuerKeyID  = "scoped-live-v1"
)

type options struct {
	repoRoot                                              string
	kubeconfig, contextName, namespace, excludedNamespace string
	preparationNamespace, postgresURL, postgresConfirm    string
	cellnBinary, cellnRoot, cellnRuntimeDir               string
	cellnTokenFile, scopedOperatorTokenFile               string
	gatewayOperatorTokenFile                              string
	packagePath, metadataPath                             string
	timeout                                               time.Duration
}

type liveInput struct {
	APIVersion string `json:"apiVersion"`
	// PackageHash is BLAKE3 over artifactPackage/package.json. The receiver
	// remains responsible for verifying every signed member in Celln's stores.
	PackageHash  string                      `json:"packageHash"`
	Runtime      api.CellnRuntimeProfileSpec `json:"runtime"`
	Task         string                      `json:"task"`
	SystemPrompt string                      `json:"systemPrompt"`
	Model        struct {
		Provider string `json:"provider"`
		Protocol string `json:"protocol"`
		Name     string `json:"name"`
	} `json:"model"`
	Expected struct {
		Output               string `json:"output"`
		ProviderCalls        int64  `json:"providerCalls"`
		ObservedOutputTokens int64  `json:"observedOutputTokens"`
		ReservedOutputTokens int64  `json:"reservedOutputTokens"`
	} `json:"expected"`
}

type createdObjects struct {
	evaluation, preparation *corev1.Namespace
	profile                 *api.CellnRuntimeProfile
	policy                  *api.CellnExecutionPolicy
	secret                  *corev1.Secret
	run                     *api.AgentRun
}

type providerRecorder struct {
	secret   string
	input    liveInput
	attempts atomic.Int64
	valid    atomic.Int64
	mu       sync.Mutex
	failure  string
}

func main() {
	var o options
	flag.StringVar(&o.repoRoot, "repo", "", "absolute Sympozium repository root")
	flag.StringVar(&o.kubeconfig, "kubeconfig", "", "absolute private kubeconfig")
	flag.StringVar(&o.contextName, "context", "", "explicit kubeconfig context")
	flag.StringVar(&o.namespace, "namespace", "", "new caller-owned evaluation namespace")
	flag.StringVar(&o.excludedNamespace, "global-controller-excludes-namespace", "", "must exactly repeat --namespace after the coordinator excludes it from every existing controller")
	flag.StringVar(&o.preparationNamespace, "preparation-namespace", "", "new protected preparation namespace")
	flag.StringVar(&o.postgresURL, "postgres-url", "", "disposable, initially empty PostgreSQL database URL")
	flag.StringVar(&o.postgresConfirm, "disposable-postgres-confirmation", "", "must exactly repeat --namespace")
	flag.StringVar(&o.cellnBinary, "celln-binary", "", "absolute current Celln binary")
	flag.StringVar(&o.cellnRoot, "celln-root", "", "absolute caller-prepared Celln authority/store root")
	flag.StringVar(&o.cellnRuntimeDir, "celln-runtime-dir", "", "optional absolute Celln runtime directory")
	flag.StringVar(&o.cellnTokenFile, "celln-token-file", "", "existing CLI dispatcher transport credential file")
	flag.StringVar(&o.scopedOperatorTokenFile, "scoped-operator-token-file", "", "existing CLI scoped operator transport credential file")
	flag.StringVar(&o.gatewayOperatorTokenFile, "gateway-operator-token-file", "", "existing private gateway operator transport credential file")
	flag.StringVar(&o.packagePath, "artifact-package", "", "absolute genuine signed runtime package directory")
	flag.StringVar(&o.metadataPath, "package-metadata", "", "absolute public harness metadata JSON")
	flag.DurationVar(&o.timeout, "timeout", 3*time.Minute, "complete harness deadline")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), o.timeout)
	defer cancel()
	if err := run(ctx, o); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: celln scoped live harness: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, o options) (retErr error) {
	input, err := validateInputs(o)
	if err != nil {
		return err
	}
	k8sClient, clientset, scheme, err := liveClient(o)
	if err != nil {
		return fmt.Errorf("live Kubernetes client: %w", err)
	}
	if err := requireAbsent(ctx, k8sClient, o); err != nil {
		return err
	}

	pool, budgets, authorities, err := openDisposablePostgres(ctx, o.postgresURL, o.repoRoot)
	if err != nil {
		return err
	}
	defer pool.Close()

	work, err := os.MkdirTemp("", "sympozium-celln-scoped-live.")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	if err := os.Chmod(work, 0700); err != nil {
		return err
	}

	publicKey, _, keyFile, jwksFile, err := writeIssuerMaterial(work)
	if err != nil {
		return err
	}
	verifier, err := cellncapability.NewVerifier(issuerName, []cellncapability.VerificationKey{{KeyID: issuerKeyID, PublicKey: publicKey}}, nil)
	if err != nil {
		return err
	}
	registrationRaw, err := readBounded(o.gatewayOperatorTokenFile, 64<<10)
	if err != nil {
		return fmt.Errorf("read gateway operator credential: %w", err)
	}
	registrationToken := strings.TrimSpace(string(registrationRaw))
	if registrationToken == "" || strings.ContainsAny(registrationToken, "\r\n") {
		return errors.New("gateway operator credential is malformed")
	}

	providerSecret, err := randomBearer()
	if err != nil {
		return err
	}
	recorder := &providerRecorder{secret: providerSecret, input: input}
	provider := newTLSServer(recorder)
	defer provider.Close()
	providerRoots, _, err := serverTrust(provider)
	if err != nil {
		return err
	}

	gateway, err := modelgateway.New(modelgateway.Config{
		AuthorityReady: modelgateway.AuthorityReadiness(clientset.AuthorizationV1().SelfSubjectAccessReviews()),
		ClusterID:      o.contextName, RegistrationToken: cellncapability.NewToken(registrationToken),
		MaxRequestBytes: 1 << 20, MaxResponseBytes: 1 << 20, MaxConcurrent: 2,
		MaxProviderDuration: 20 * time.Second, AllowPrivateOrigins: map[string]bool{provider.URL: true},
		ProviderRootCAs: providerRoots,
	}, verifier, k8sClient, budgets, authorities)
	if err != nil {
		return fmt.Errorf("construct real gateway: %w", err)
	}
	if err := gateway.Ready(ctx); err != nil {
		return fmt.Errorf("real gateway readiness (live Kubernetes authority and PostgreSQL): %w", err)
	}
	gatewayServer := newTLSServer(gateway.Handler())
	defer gatewayServer.Close()
	_, gatewayCA, err := serverTrust(gatewayServer)
	if err != nil {
		return err
	}
	gatewayCAFile := filepath.Join(work, "gateway-ca.pem")
	if err := os.WriteFile(gatewayCAFile, gatewayCA, 0644); err != nil {
		return err
	}
	dispatchAddress, err := unusedLoopbackAddress()
	if err != nil {
		return err
	}
	cellnCmd, err := startCelln(ctx, o, dispatchAddress, jwksFile, gatewayServer.URL, gatewayCAFile)
	if err != nil {
		return err
	}
	defer stopProcess(cellnCmd)
	if err := waitForListener(ctx, dispatchAddress, cellnCmd); err != nil {
		return err
	}

	receiverTarget, _ := url.Parse("http://" + dispatchAddress)
	receiverProxy := httputil.NewSingleHostReverseProxy(receiverTarget)
	receiverProxy.ErrorLog = log.New(io.Discard, "", 0)
	receiverTLS := newTLSServer(receiverProxy)
	defer receiverTLS.Close()
	_, receiverCA, err := serverTrust(receiverTLS)
	if err != nil {
		return err
	}
	receiverCAFile := filepath.Join(work, "receiver-ca.pem")
	if err := os.WriteFile(receiverCAFile, receiverCA, 0644); err != nil {
		return err
	}

	configFile := filepath.Join(work, "scoped-controller.json")
	if err := writeControllerConfig(configFile, o, keyFile, receiverTLS.URL, receiverCAFile, gatewayServer.URL, gatewayCAFile, o.gatewayOperatorTokenFile); err != nil {
		return err
	}
	dispatcher, err := cellnscoped.LoadDispatcher(configFile, k8sClient, k8sClient)
	if err != nil {
		return fmt.Errorf("load concrete scoped dispatcher: %w", err)
	}

	objects, err := createAuthorityAndRun(ctx, k8sClient, o, input, provider.URL, providerSecret)
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if cleanupErr := cleanupCreated(cleanupCtx, k8sClient, objects); cleanupErr != nil {
			return fmt.Errorf("%v; rollback: %w", err, cleanupErr)
		}
		return err
	}
	reconciler := &controller.AgentRunReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: scheme, Log: logr.Discard(), ScopedDispatcher: dispatcher, RunHistoryLimit: 1000}
	cleanupNeeded := true
	defer func() {
		if !cleanupNeeded {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var cleanupFailures []string
		if cleanupErr := deleteRunThroughController(cleanupCtx, reconciler, k8sClient, objects.run); cleanupErr != nil {
			cleanupFailures = append(cleanupFailures, cleanupErr.Error())
		}
		if cleanupErr := cleanupCreated(cleanupCtx, k8sClient, objects); cleanupErr != nil {
			cleanupFailures = append(cleanupFailures, cleanupErr.Error())
		}
		if len(cleanupFailures) != 0 {
			retErr = fmt.Errorf("%v; failure cleanup: %s", retErr, strings.Join(cleanupFailures, "; "))
		}
	}()

	terminal, duplicate, final, err := driveToTerminal(ctx, reconciler, dispatcher, k8sClient, objects.run)
	if err != nil {
		return err
	}
	if terminal.Status.Result != input.Expected.Output || duplicate.Output != terminal.Status.Result || duplicate.ReceiptDigest != terminal.Status.CellnScoped.ReceiptDigest {
		return fmt.Errorf("result/duplicate mismatch: phase=%s resultMatch=%t duplicateMatch=%t", terminal.Status.Phase, terminal.Status.Result == input.Expected.Output, duplicate.Output == terminal.Status.Result)
	}
	if recorder.attempts.Load() != input.Expected.ProviderCalls || recorder.valid.Load() != input.Expected.ProviderCalls {
		return fmt.Errorf("provider calls mismatch: attempts=%d valid=%d expected=%d diagnostic=%s", recorder.attempts.Load(), recorder.valid.Load(), input.Expected.ProviderCalls, recorder.failureMessage())
	}
	decision, err := cellnscoped.CapabilityDecision(final.Decision)
	if err != nil {
		return err
	}
	if decision.Route.CredentialSource == nil || decision.Route.CredentialSource.SecretUID != string(objects.secret.UID) || decision.Route.CredentialSource.SecretName != objects.secret.Name || decision.Route.CredentialSource.SecretKey != "OPENAI_API_KEY" {
		return errors.New("final route did not pin the namespace-specific test Secret identity")
	}
	if err := assertProtectedRecords(ctx, k8sClient, o.preparationNamespace, terminal, providerSecret); err != nil {
		return err
	}
	usage, err := budgets.Inspect(ctx, decision.Budget.BudgetID, decision.Run.UID)
	if err != nil {
		return fmt.Errorf("inspect PostgreSQL budget before cleanup: %w", err)
	}
	if usage.RunReservedRequests != input.Expected.ProviderCalls || usage.TurnReservedRequests != input.Expected.ProviderCalls || usage.RunObservedOutputTokens != input.Expected.ObservedOutputTokens || usage.TurnObservedOutputTokens != input.Expected.ObservedOutputTokens || usage.RunReservedOutputTokens != input.Expected.ReservedOutputTokens || usage.TurnReservedOutputTokens != input.Expected.ReservedOutputTokens || usage.RunClosed || usage.TurnClosed {
		return fmt.Errorf("unexpected PostgreSQL budget before cleanup: %+v", usage)
	}
	cleaned, err := driveCleanup(ctx, reconciler, k8sClient, objects.run)
	if err != nil {
		return err
	}
	usage, err = budgets.Inspect(ctx, decision.Budget.BudgetID, decision.Run.UID)
	if err != nil {
		return fmt.Errorf("inspect PostgreSQL budget after cleanup: %w", err)
	}
	if !cleaned.Status.CellnScoped.CleanupConfirmed || containsFinalizer(cleaned.Finalizers) || !usage.RunClosed || !usage.TurnClosed {
		return fmt.Errorf("cleanup not confirmed: native=%t finalizer=%t runBudgetClosed=%t turnBudgetClosed=%t", cleaned.Status.CellnScoped.CleanupConfirmed, containsFinalizer(cleaned.Finalizers), usage.RunClosed, usage.TurnClosed)
	}
	var jobs batchv1.JobList
	if err := k8sClient.List(ctx, &jobs, client.InNamespace(o.namespace)); err != nil || len(jobs.Items) != 0 {
		return fmt.Errorf("scoped path created Kubernetes jobs: count=%d error=%v", len(jobs.Items), err)
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := cleanupCreated(cleanupCtx, k8sClient, objects); err != nil {
		cleanupCancel()
		return err
	}
	cleanupCancel()
	cleanupNeeded = false

	summary := map[string]any{
		"suite": "controller-native-celln-tls-gateway-postgresql", "status": "passed",
		"namespace": o.namespace, "runUid": string(cleaned.UID), "receiverId": cleaned.Status.CellnScoped.ReceiverID,
		"owner": cleaned.Status.CellnScoped.Owner, "receiptDigest": cleaned.Status.CellnScoped.ReceiptDigest,
		"providerCalls": recorder.valid.Load(), "reservedOutputTokens": usage.RunReservedOutputTokens,
		"observedOutputTokens": usage.RunObservedOutputTokens, "nativeCleanupConfirmed": true,
		"gatewayBudgetClosed": true, "duplicateSuppressed": true, "packageHash": input.PackageHash,
	}
	encoded, _ := json.Marshal(summary)
	fmt.Println(string(encoded))
	return nil
}

func validateInputs(o options) (liveInput, error) {
	var in liveInput
	if o.excludedNamespace == "" || o.excludedNamespace != o.namespace {
		return in, errors.New("refusing to create AgentRuns: --global-controller-excludes-namespace must exactly equal --namespace after coordinator isolation")
	}
	if o.postgresConfirm == "" || o.postgresConfirm != o.namespace {
		return in, errors.New("--disposable-postgres-confirmation must exactly equal --namespace")
	}
	if len(validation.IsDNS1123Label(o.namespace)) != 0 || len(o.namespace) > 48 || len(validation.IsDNS1123Label(o.preparationNamespace)) != 0 || o.preparationNamespace == o.namespace {
		return in, errors.New("distinct DNS-label evaluation/preparation namespaces are required (evaluation max 48 bytes)")
	}
	for name, value := range map[string]string{"repo": o.repoRoot, "kubeconfig": o.kubeconfig, "celln-binary": o.cellnBinary, "celln-root": o.cellnRoot, "celln-token-file": o.cellnTokenFile, "scoped-operator-token-file": o.scopedOperatorTokenFile, "gateway-operator-token-file": o.gatewayOperatorTokenFile, "artifact-package": o.packagePath, "package-metadata": o.metadataPath} {
		if !filepath.IsAbs(value) {
			return in, fmt.Errorf("--%s must be absolute", name)
		}
	}
	if o.contextName == "" || o.postgresURL == "" || o.timeout < 30*time.Second || o.timeout > 15*time.Minute {
		return in, errors.New("explicit context/PostgreSQL URL and a 30s-15m timeout are required")
	}
	if o.cellnRuntimeDir != "" && !filepath.IsAbs(o.cellnRuntimeDir) {
		return in, errors.New("--celln-runtime-dir must be absolute")
	}
	if err := privateRegular(o.kubeconfig); err != nil {
		return in, fmt.Errorf("private kubeconfig: %w", err)
	}
	for _, path := range []string{o.cellnTokenFile, o.scopedOperatorTokenFile, o.gatewayOperatorTokenFile} {
		if err := privateRegular(path); err != nil {
			return in, fmt.Errorf("private operator credential file: %w", err)
		}
	}
	info, err := os.Stat(o.cellnBinary)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
		return in, errors.New("celln binary must be an executable regular file")
	}
	if info, err = os.Stat(o.cellnRoot); err != nil || !info.IsDir() {
		return in, errors.New("celln root must be an existing directory")
	}
	if _, err := os.Stat(filepath.Join(o.cellnRoot, "scoped")); !os.IsNotExist(err) {
		return in, errors.New("celln root already contains scoped receiver state; a fresh caller-owned root is required")
	}
	raw, err := readBounded(o.metadataPath, 256<<10)
	if err != nil {
		return in, err
	}
	if err := cellncapability.StrictDecode(raw, &in); err != nil {
		return in, fmt.Errorf("invalid package metadata: %w", err)
	}
	manifest, err := readBounded(filepath.Join(o.packagePath, "package.json"), 256<<10)
	if err != nil {
		return in, fmt.Errorf("read signed package manifest: %w", err)
	}
	actual := fmt.Sprintf("blake3:%x", blake3.Sum256(manifest))
	if in.APIVersion != inputVersion || in.PackageHash != actual {
		return in, fmt.Errorf("metadata/package mismatch: apiVersion=%q hashMatch=%t", in.APIVersion, in.PackageHash == actual)
	}
	if in.Runtime.ContractVersion != "celln.json-tools/v1" || in.Runtime.JSON == nil || len(in.Runtime.Lifecycles) == 0 || in.Task == "" || len(in.Task) > 2048 || in.Expected.Output == "" || in.Expected.ProviderCalls != 1 || in.Expected.ObservedOutputTokens < 0 || in.Expected.ReservedOutputTokens != 512 || in.Expected.ReservedOutputTokens < in.Expected.ObservedOutputTokens || in.Model.Protocol != "openai-chat" || in.Model.Provider == "" || in.Model.Name == "" {
		return in, errors.New("metadata must describe one bounded model-only openai-chat JSON runtime call and exact expected accounting")
	}
	return in, nil
}
