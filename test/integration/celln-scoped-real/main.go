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
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	issuerName  = "sympozium-control-plane"
	issuerKeyID = "scoped-live-v1"
)

type options struct {
	repoRoot                                                                                   string
	kubeconfig, contextName, namespace, enduringNamespace, deniedNamespace, excludedNamespaces string
	preparationNamespace, postgresURL, postgresConfirm                                         string
	reviewOwnership                                                                            string
	cellnBinary, cellnRoot, cellnRuntimeDir                                                    string
	cellnTokenFile, scopedOperatorTokenFile                                                    string
	gatewayOperatorTokenFile                                                                   string
	packagePath                                                                                string
	timeout                                                                                    time.Duration
}

const (
	modelProvider        = "scoped-fixture"
	modelProtocol        = "openai-chat"
	modelName            = "uppercase-fixture"
	modelTask            = "Call uppercase with text celln. Wait for its result, then answer only the returned text."
	directArguments      = `{"text":"celln"}`
	directOutput         = `{"text":"CELLN"}`
	uppercaseOutput      = "CELLN"
	requestReservation   = int64(512)
	observedPerExecution = int64(3)
)

type createdObjects struct {
	preparationNamespace string
	namespaces           []*corev1.Namespace
	profiles             []*api.CellnRuntimeProfile
	tool                 *api.ClusterCellnTool
	policy               *api.CellnExecutionPolicy
	namespaced           []client.Object
	protected            []client.Object
	runs                 []*api.AgentRun
}

type providerRecorder struct {
	oneShotSecret, enduringSecret string
	attempts                      atomic.Int64
	valid                         atomic.Int64
	mu                            sync.Mutex
	failure                       string
}

func main() {
	// controller-runtime registers its own global kubeconfig flag; this harness
	// owns an explicit client configuration and must not share that flag set.
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	var o options
	flag.StringVar(&o.repoRoot, "repo", "", "absolute Sympozium repository root")
	flag.StringVar(&o.kubeconfig, "kubeconfig", "", "absolute private kubeconfig")
	flag.StringVar(&o.contextName, "context", "", "explicit kubeconfig context")
	flag.StringVar(&o.namespace, "namespace", "", "isolated direct/one-shot evaluation namespace")
	flag.StringVar(&o.enduringNamespace, "enduring-namespace", "", "isolated enduring evaluation namespace")
	flag.StringVar(&o.deniedNamespace, "denied-namespace", "", "isolated namespace without tenant enablement")
	flag.StringVar(&o.excludedNamespaces, "global-controller-excludes-namespaces", "", "comma-separated exact evaluation, enduring, denied, and preparation namespaces excluded from the global manager")
	flag.StringVar(&o.preparationNamespace, "preparation-namespace", "", "protected preparation namespace")
	flag.StringVar(&o.reviewOwnership, "existing-review-ownership", "", "opt in to empty pre-created namespaces carrying sympozium.ai/celln-review=<value>")
	flag.StringVar(&o.postgresURL, "postgres-url", "", "disposable, initially empty PostgreSQL database URL")
	flag.StringVar(&o.postgresConfirm, "disposable-postgres-confirmation", "", "must exactly repeat --namespace")
	flag.StringVar(&o.cellnBinary, "celln-binary", "", "absolute current Celln binary")
	flag.StringVar(&o.cellnRoot, "celln-root", "", "absolute caller-prepared Celln authority/store root")
	flag.StringVar(&o.cellnRuntimeDir, "celln-runtime-dir", "", "optional absolute Celln runtime directory")
	flag.StringVar(&o.cellnTokenFile, "celln-token-file", "", "existing CLI dispatcher transport credential file")
	flag.StringVar(&o.scopedOperatorTokenFile, "scoped-operator-token-file", "", "existing CLI scoped operator transport credential file")
	flag.StringVar(&o.gatewayOperatorTokenFile, "gateway-operator-token-file", "", "existing private gateway operator transport credential file")
	flag.StringVar(&o.packagePath, "artifact-package", "", "absolute genuine signed runtime package directory")
	flag.DurationVar(&o.timeout, "timeout", 10*time.Minute, "complete harness deadline")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), o.timeout)
	defer cancel()
	if err := run(ctx, o); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: celln scoped live harness: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, o options) (retErr error) {
	pkg, err := validateInputs(o)
	if err != nil {
		return err
	}
	k8sClient, clientset, scheme, err := liveClient(o)
	if err != nil {
		return fmt.Errorf("live Kubernetes client: %w", err)
	}
	if err := requireAbsent(ctx, k8sClient, o, pkg); err != nil {
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
	defer func() {
		if retErr == nil {
			_ = os.RemoveAll(work)
		} else {
			fmt.Fprintf(os.Stderr, "Private recovery material retained at %s; do not remove until native cleanup is confirmed\n", work)
		}
	}()
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

	oneShotProviderSecret, err := randomBearer()
	if err != nil {
		return err
	}
	enduringProviderSecret, err := randomBearer()
	if err != nil {
		return err
	}
	recorder := &providerRecorder{oneShotSecret: oneShotProviderSecret, enduringSecret: enduringProviderSecret}
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
	parentTemplate := filepath.Join(work, "scoped-parent-template.json")
	if err := writeParentTemplate(parentTemplate, pkg); err != nil {
		return err
	}
	cellnCmd, err := startCelln(ctx, o, dispatchAddress, jwksFile, gatewayServer.URL, gatewayCAFile, parentTemplate)
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

	objects, runs, err := createAuthorityAndRuns(ctx, k8sClient, o, pkg, provider.URL, oneShotProviderSecret, enduringProviderSecret)
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if cleanupErr := cleanupCreated(cleanupCtx, k8sClient, objects); cleanupErr != nil {
			return fmt.Errorf("%v; rollback: %w", err, cleanupErr)
		}
		return err
	}
	reconciler := &controller.AgentRunReconciler{Client: k8sClient, APIReader: k8sClient, Scheme: scheme, Log: logr.Discard(), ScopedDispatcher: dispatcher, RunHistoryLimit: 1000}
	turnReconciler := &controller.AgentRunTurnReconciler{Client: k8sClient, APIReader: k8sClient, ScopedDispatcher: dispatcher}
	cleanupNeeded := true
	defer func() {
		if !cleanupNeeded {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var cleanupFailures []string
		for _, run := range objects.runs {
			var current api.AgentRun
			if run != nil && k8sClient.Get(cleanupCtx, client.ObjectKeyFromObject(run), &current) == nil && current.UID == run.UID && current.Status.CellnScoped != nil {
				if err := rememberProtected(cleanupCtx, k8sClient, objects, o.preparationNamespace, current.Status.CellnScoped); err != nil {
					cleanupFailures = append(cleanupFailures, "protected recovery identity could not be retained")
				}
			}
			if run != nil && current.UID == run.UID && current.UID != "" && current.Spec.ExecutionLifecycle == "enduring" && current.Status.CellnScoped != nil {
				if !current.Status.CellnScoped.CleanupConfirmed {
					if _, err := beginEnduringCleanup(cleanupCtx, reconciler, k8sClient, &current); err != nil {
						cleanupFailures = append(cleanupFailures, err.Error())
						continue
					}
				}
				var children api.AgentRunTurnList
				if err := k8sClient.List(cleanupCtx, &children, client.InNamespace(current.Namespace)); err != nil {
					cleanupFailures = append(cleanupFailures, err.Error())
					continue
				}
				childFailed := false
				for i := range children.Items {
					child := &children.Items[i]
					if child.Spec.RunUID != string(current.UID) || child.Spec.RunName != current.Name {
						continue
					}
					if err := rememberProtected(cleanupCtx, k8sClient, objects, o.preparationNamespace, child.Status.CellnScoped); err != nil {
						cleanupFailures = append(cleanupFailures, err.Error())
						childFailed = true
						continue
					}
					if err := finishTurnAfterRootCleanup(cleanupCtx, turnReconciler, k8sClient, child); err != nil {
						cleanupFailures = append(cleanupFailures, err.Error())
						childFailed = true
					}
				}
				if childFailed {
					continue
				}
			}
			if cleanupErr := deleteRunThroughController(cleanupCtx, reconciler, k8sClient, run); cleanupErr != nil {
				cleanupFailures = append(cleanupFailures, cleanupErr.Error())
			}
		}
		// Preserve protected identity and all recovery dependencies whenever any
		// native cleanup is uncertain. UID-scoped deletion alone is not enough.
		if len(cleanupFailures) == 0 {
			if cleanupErr := cleanupCreated(cleanupCtx, k8sClient, objects); cleanupErr != nil {
				cleanupFailures = append(cleanupFailures, cleanupErr.Error())
			}
		}
		if len(cleanupFailures) != 0 {
			retErr = fmt.Errorf("%v; failure cleanup: %s", retErr, strings.Join(cleanupFailures, "; "))
		}
	}()

	if err := assertDeniedNamespace(ctx, k8sClient, o, runs.denied); err != nil {
		return err
	}
	direct, _, directFinal, err := driveToTerminal(ctx, reconciler, dispatcher, k8sClient, runs.direct)
	if err != nil {
		return err
	}
	if direct.Status.Result != directOutput || recorder.attempts.Load() != 0 || directFinal.Decision.Route.Provider != "none" {
		return errors.New("model-free direct uppercase execution did not return the real tool output without provider use")
	}
	if err := assertNativeProvenance(direct.Status.CellnScoped, false); err != nil {
		return fmt.Errorf("direct provenance: %w", err)
	}
	if err := rememberProtected(ctx, k8sClient, objects, o.preparationNamespace, direct.Status.CellnScoped); err != nil {
		return err
	}
	if _, err = driveCleanup(ctx, reconciler, k8sClient, runs.direct); err != nil {
		return err
	}

	oneShot, duplicate, oneFinal, err := driveToTerminal(ctx, reconciler, dispatcher, k8sClient, runs.oneShot)
	if err != nil {
		return err
	}
	if oneShot.Status.Result != uppercaseOutput || duplicate.Output != uppercaseOutput || duplicate.ReceiptDigest != oneShot.Status.CellnScoped.ReceiptDigest || recorder.valid.Load() != 2 {
		return fmt.Errorf("composed one-shot uppercase mismatch: result=%q calls=%d diagnostic=%s", oneShot.Status.Result, recorder.valid.Load(), recorder.failureMessage())
	}
	if err := assertNativeProvenance(oneShot.Status.CellnScoped, false); err != nil {
		return fmt.Errorf("one-shot provenance: %w", err)
	}
	oneDecision, err := cellnscoped.CapabilityDecision(oneFinal.Decision)
	if err != nil {
		return err
	}
	oneSecret := runs.oneShotSecret
	if oneDecision.Route.CredentialSource == nil || oneDecision.Route.CredentialSource.SecretUID != string(oneSecret.UID) || oneDecision.Route.CredentialSource.SecretName != oneSecret.Name || oneDecision.Route.CredentialSource.SecretKey != "OPENAI_API_KEY" {
		return errors.New("one-shot route did not pin its namespace Secret UID")
	}
	if err := assertProtectedRecords(ctx, k8sClient, o.preparationNamespace, oneShot, oneShotProviderSecret, enduringProviderSecret); err != nil {
		return err
	}
	if err := rememberProtected(ctx, k8sClient, objects, o.preparationNamespace, oneShot.Status.CellnScoped); err != nil {
		return err
	}
	oneUsage, err := budgets.Inspect(ctx, oneDecision.Budget.BudgetID, oneDecision.Run.UID)
	if err != nil {
		return err
	}
	if err := exactUsage(oneUsage, 2, 1024, observedPerExecution, 2, 1024, observedPerExecution, false, false); err != nil {
		return fmt.Errorf("one-shot accounting: %w", err)
	}
	oneCleaned, err := driveCleanup(ctx, reconciler, k8sClient, runs.oneShot)
	if err != nil {
		return err
	}
	oneUsage, err = budgets.Inspect(ctx, oneDecision.Budget.BudgetID, oneDecision.Run.UID)
	if err != nil {
		return err
	}
	if err := exactUsage(oneUsage, 2, 1024, observedPerExecution, 2, 1024, observedPerExecution, true, true); err != nil {
		return fmt.Errorf("one-shot closed accounting: %w", err)
	}

	enduring, enduringFinal, err := driveEnduringReady(ctx, reconciler, dispatcher, k8sClient, runs.enduring)
	if err != nil {
		return err
	}
	if enduring.Status.Result != uppercaseOutput || recorder.valid.Load() != 4 {
		return fmt.Errorf("enduring initial uppercase mismatch: result=%q calls=%d", enduring.Status.Result, recorder.valid.Load())
	}
	if err := assertNativeProvenance(enduring.Status.CellnScoped, true); err != nil {
		return fmt.Errorf("enduring root provenance: %w", err)
	}
	if err := assertProtectedRecords(ctx, k8sClient, o.preparationNamespace, enduring, oneShotProviderSecret, enduringProviderSecret); err != nil {
		return err
	}
	if err := rememberProtected(ctx, k8sClient, objects, o.preparationNamespace, enduring.Status.CellnScoped); err != nil {
		return err
	}
	turn, err := createAndDriveTurn(ctx, turnReconciler, k8sClient, &enduring, "turn-one", "Call uppercase with text violet and answer only the returned text.")
	if err != nil {
		return err
	}
	objects.namespaced = append(objects.namespaced, turn.DeepCopy())
	if turn.Status.Execution == nil || turn.Status.Execution.Result == nil || !turn.Status.Execution.Result.Succeeded || turn.Status.Execution.Result.Answer != "VIOLET" || recorder.valid.Load() != 6 {
		return fmt.Errorf("enduring turn output/accounting mismatch: calls=%d", recorder.valid.Load())
	}
	if err := assertNativeProvenance(turn.Status.CellnScoped, true); err != nil {
		return fmt.Errorf("enduring turn provenance: %w", err)
	}
	if err := rememberProtected(ctx, k8sClient, objects, o.preparationNamespace, turn.Status.CellnScoped); err != nil {
		return err
	}
	endDecision, err := cellnscoped.CapabilityDecision(enduringFinal.Decision)
	if err != nil {
		return err
	}
	endSecret := runs.enduringSecret
	if endDecision.Route.CredentialSource == nil || endDecision.Route.CredentialSource.SecretUID != string(endSecret.UID) || endDecision.Route.CredentialSource.SecretName != endSecret.Name || endDecision.Route.CredentialSource.SecretKey != "OPENAI_API_KEY" || endDecision.Route.CredentialSource.SecretUID == oneDecision.Route.CredentialSource.SecretUID {
		return errors.New("enduring route did not pin its isolated namespace Secret UID")
	}
	rootUsage, err := budgets.Inspect(ctx, endDecision.Budget.BudgetID, endDecision.Run.UID)
	if err != nil {
		return err
	}
	if err := exactUsage(rootUsage, 4, 2048, 2*observedPerExecution, 2, 1024, observedPerExecution, false, false); err != nil {
		return fmt.Errorf("shared enduring root accounting: %w", err)
	}
	turnUsage, err := budgets.Inspect(ctx, endDecision.Budget.BudgetID, string(turn.UID))
	if err != nil {
		return err
	}
	if err := exactUsage(turnUsage, 4, 2048, 2*observedPerExecution, 2, 1024, observedPerExecution, false, true); err != nil {
		return fmt.Errorf("enduring turn accounting: %w", err)
	}
	exhausted, err := createAndProveExhaustedTurn(ctx, turnReconciler, k8sClient, &enduring, recorder)
	if err != nil {
		return err
	}
	objects.namespaced = append(objects.namespaced, exhausted.DeepCopy())
	if err := rememberProtected(ctx, k8sClient, objects, o.preparationNamespace, exhausted.Status.CellnScoped); err != nil {
		return err
	}
	endCleaned, err := beginEnduringCleanup(ctx, reconciler, k8sClient, runs.enduring)
	if err != nil {
		return err
	}
	if err := finishTurnAfterRootCleanup(ctx, turnReconciler, k8sClient, exhausted); err != nil {
		return err
	}
	if err := deleteRunThroughController(ctx, reconciler, k8sClient, runs.enduring); err != nil {
		return err
	}
	rootUsage, err = budgets.Inspect(ctx, endDecision.Budget.BudgetID, endDecision.Run.UID)
	if err != nil {
		return err
	}
	if err := exactUsage(rootUsage, 4, 2048, 2*observedPerExecution, 2, 1024, observedPerExecution, true, true); err != nil {
		return fmt.Errorf("enduring closed accounting: %w", err)
	}
	if endCleaned.Status.CellnScoped == nil || !endCleaned.Status.CellnScoped.CleanupConfirmed || oneCleaned.Status.CellnScoped == nil || !oneCleaned.Status.CellnScoped.CleanupConfirmed || containsFinalizer(oneCleaned.Finalizers) {
		return errors.New("native cleanup/finalizer confirmation missing")
	}
	for _, namespace := range []string{o.namespace, o.enduringNamespace, o.deniedNamespace} {
		var jobs batchv1.JobList
		if err := k8sClient.List(ctx, &jobs, client.InNamespace(namespace)); err != nil || len(jobs.Items) != 0 {
			return fmt.Errorf("scoped path created Kubernetes jobs in %s: count=%d error=%v", namespace, len(jobs.Items), err)
		}
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := cleanupCreated(cleanupCtx, k8sClient, objects); err != nil {
		cleanupCancel()
		return err
	}
	cleanupCancel()
	cleanupNeeded = false
	if o.reviewOwnership != "" {
		for _, namespace := range []string{o.namespace, o.enduringNamespace, o.deniedNamespace, o.preparationNamespace} {
			if err := requireReviewNamespaceEmpty(ctx, k8sClient, namespace); err != nil {
				return fmt.Errorf("post-cleanup inventory: %w", err)
			}
		}
	}

	summary := map[string]any{
		"suite": "controller-native-celln-tls-gateway-postgresql", "status": "passed",
		"namespaces": []string{o.namespace, o.enduringNamespace, o.deniedNamespace, o.preparationNamespace}, "oneShotRunUid": string(oneCleaned.UID), "enduringRunUid": string(endCleaned.UID),
		"providerCalls": recorder.valid.Load(), "enduringReservedOutputTokens": rootUsage.RunReservedOutputTokens,
		"enduringObservedOutputTokens": rootUsage.RunObservedOutputTokens, "nativeCleanupConfirmed": true,
		"gatewayBudgetClosed": true, "duplicateSuppressed": true, "budgetExhaustionProved": true, "packageHash": pkg.Hash,
	}
	encoded, _ := json.Marshal(summary)
	fmt.Println(string(encoded))
	return nil
}

func validateInputs(o options) (nativePackage, error) {
	var pkg nativePackage
	wantExcluded := strings.Join([]string{o.namespace, o.enduringNamespace, o.deniedNamespace, o.preparationNamespace}, ",")
	if o.excludedNamespaces == "" || o.excludedNamespaces != wantExcluded {
		return pkg, fmt.Errorf("refusing AgentRuns: --global-controller-excludes-namespaces must exactly equal %q", wantExcluded)
	}
	if o.postgresConfirm == "" || o.postgresConfirm != o.namespace {
		return pkg, errors.New("--disposable-postgres-confirmation must exactly equal --namespace")
	}
	namespaces := []string{o.namespace, o.enduringNamespace, o.deniedNamespace, o.preparationNamespace}
	seen := map[string]bool{}
	for _, name := range namespaces {
		if len(validation.IsDNS1123Label(name)) != 0 || len(name) > 48 || seen[name] {
			return pkg, errors.New("four distinct DNS-label namespaces of at most 48 bytes are required")
		}
		seen[name] = true
	}
	for name, value := range map[string]string{"repo": o.repoRoot, "kubeconfig": o.kubeconfig, "celln-binary": o.cellnBinary, "celln-root": o.cellnRoot, "celln-token-file": o.cellnTokenFile, "scoped-operator-token-file": o.scopedOperatorTokenFile, "gateway-operator-token-file": o.gatewayOperatorTokenFile, "artifact-package": o.packagePath} {
		if !filepath.IsAbs(value) {
			return pkg, fmt.Errorf("--%s must be absolute", name)
		}
	}
	if o.contextName == "" || o.postgresURL == "" || o.timeout < 30*time.Second || o.timeout > 15*time.Minute {
		return pkg, errors.New("explicit context/PostgreSQL URL and a 30s-15m timeout are required")
	}
	if o.cellnRuntimeDir != "" && !filepath.IsAbs(o.cellnRuntimeDir) {
		return pkg, errors.New("--celln-runtime-dir must be absolute")
	}
	if err := privateRegular(o.kubeconfig); err != nil {
		return pkg, fmt.Errorf("private kubeconfig: %w", err)
	}
	for _, path := range []string{o.cellnTokenFile, o.scopedOperatorTokenFile, o.gatewayOperatorTokenFile} {
		if err := privateRegular(path); err != nil {
			return pkg, fmt.Errorf("private operator credential file: %w", err)
		}
	}
	info, err := os.Stat(o.cellnBinary)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
		return pkg, errors.New("celln binary must be an executable regular file")
	}
	if info, err = os.Stat(o.cellnRoot); err != nil || !info.IsDir() {
		return pkg, errors.New("celln root must be an existing directory")
	}
	if _, err := os.Stat(filepath.Join(o.cellnRoot, "scoped")); !os.IsNotExist(err) {
		return pkg, errors.New("celln root already contains scoped receiver state; a fresh caller-owned root is required")
	}
	pkg, err = loadNativePackage(o.packagePath)
	if err != nil {
		return pkg, fmt.Errorf("verify actual framework package: %w", err)
	}
	return pkg, nil
}
