package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"github.com/sympozium-ai/sympozium/internal/cellninstall"
	"github.com/sympozium-ai/sympozium/internal/cellnplatform"
)

// cellnFleetFlags configure `sympozium install --celln-fleet`: one reviewed
// package, one scope, and every node labeled celln.dev/kvm=true joins.
type cellnFleetFlags struct {
	enabled             bool
	options             cellninstall.FleetOptions
	modelCredentialFile string
	outputDir           string
	authorise           string
	wait                time.Duration
}

func (f *cellnFleetFlags) register(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&f.enabled, "celln-fleet", false, "Run the native Celln plane as a per-node fleet: every node labeled celln.dev/kvm=true prepares the reviewed starter package and serves enduring parents (requires the --celln-fleet-* inputs and --celln-native-approve-starter-tools)")
	cmd.Flags().StringVar(&f.options.Scope, "celln-fleet-scope", "", "Stable installation identity; node state lives at /var/lib/sympozium-celln/<scope>")
	cmd.Flags().StringVar(&f.options.PackageImage, "celln-fleet-package-image", "", "Digest-pinned OCI image (repository@sha256:...) carrying the reviewed 'celln starter-package' output at /package")
	cmd.Flags().StringVar(&f.options.PackageHash, "celln-fleet-package-hash", "", "Exact operator-approved package BLAKE3 identity from 'celln starter-inspect'")
	cmd.Flags().StringVar(&f.options.Publisher, "celln-fleet-publisher", "", "Explicitly approved publisher key of the package from 'celln starter-inspect'")
	cmd.Flags().StringVar(&f.options.Principal, "celln-fleet-principal", "sympozium:celln", "Parent principal every fleet owner authenticates")
	cmd.Flags().StringVar(&f.options.Model.Provider, "celln-fleet-model-provider", cellninstall.ModelProviderDeepSeek, "Model backend for the starter agent: deepseek, openai, anthropic or llama-server (any other name needs --celln-fleet-model-endpoint and --celln-fleet-model-protocol)")
	cmd.Flags().StringVar(&f.options.Model.Name, "celln-fleet-model", "", "Model name the backend serves (required except for deepseek, which defaults to deepseek-chat)")
	cmd.Flags().StringVar(&f.options.Model.Endpoint, "celln-fleet-model-endpoint", "", "Full chat endpoint URL; defaults per provider (required for llama-server, e.g. http://HOST:8080/v1/chat/completions)")
	cmd.Flags().StringVar(&f.options.Model.Protocol, "celln-fleet-model-protocol", "", "openai-chat or anthropic-messages; defaults per provider")
	cmd.Flags().BoolVar(&f.options.Model.AllowInsecure, "celln-fleet-model-allow-insecure", false, "Approve a plain-HTTP or private model endpoint such as a LAN llama-server")
	cmd.Flags().Int64Var(&f.options.Limits.LeaseSeconds, "celln-fleet-max-lease-seconds", cellninstall.DefaultFleetLimits.LeaseSeconds, "Longest a parent may live (60–86400); the policy ceiling every run in the scope is admitted under")
	cmd.Flags().Int64Var(&f.options.Limits.MaxTurns, "celln-fleet-max-turns", cellninstall.DefaultFleetLimits.MaxTurns, "Most turns one parent may take (1–1024)")
	cmd.Flags().Int64Var(&f.options.Limits.MaxModelRequests, "celln-fleet-max-model-requests", cellninstall.DefaultFleetLimits.MaxModelRequests, "Most model requests one parent may make over its life (3–6144)")
	cmd.Flags().Int64Var(&f.options.Limits.MaxOutputTokens, "celln-fleet-max-output-tokens", cellninstall.DefaultFleetLimits.MaxOutputTokens, "Most model output tokens one parent may consume over its life (1536–3145728)")
	cmd.Flags().StringVar(&f.authorise, "celln-fleet-authorise", "all", "Which namespaces may run on the fleet: 'all' (every namespace except kube-*, cert-manager, the control-plane namespaces and namespaces labeled celln.sympozium.ai/excluded) or 'labeled' (only namespaces labeled celln.sympozium.ai/scope=<scope>)")
	cmd.Flags().StringVar(&f.options.ModelCredentialPath, "celln-fleet-model-credential-path", "/etc/celln-native/model-token", "Absolute path inside every dispatcher where the model credential Secret is mounted; recorded in the model profile")
	cmd.Flags().StringVar(&f.modelCredentialFile, "celln-fleet-model-credential-file", "", "Local file holding the model provider credential to publish once as a Secret in celln-system (omit to keep an existing Secret; not needed for llama-server)")
	cmd.Flags().StringVar(&f.outputDir, "celln-fleet-output-dir", "", "Absolute private directory for the materialized configuration and installation records")
	cmd.Flags().DurationVar(&f.wait, "celln-fleet-wait", 15*time.Minute, "How long to wait for the first labeled node to publish the starter configuration")
}

// installCellnFleet runs the two-phase fleet installation: deploy the plane so
// labeled nodes prepare themselves, then bind the catalogue and controller to
// the configuration the nodes published. Every step refuses to replace state
// left by an earlier attempt, so a rerun after labeling more nodes is safe.
func installCellnFleet(ctx context.Context, f cellnFleetFlags, imageTag string, setValues []string, approve bool) error {
	if !approve {
		return fmt.Errorf("--celln-fleet requires --celln-native-approve-starter-tools: grants include run-owned read/write and bounded example.com HTTPS")
	}
	fleetValues, err := cellninstall.FleetValues(f.options)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(f.outputDir) || filepath.Clean(f.outputDir) != f.outputDir {
		return fmt.Errorf("--celln-fleet-output-dir must be a clean absolute directory")
	}
	if err := os.MkdirAll(f.outputDir, 0700); err != nil {
		return err
	}
	if err := initClient(); err != nil {
		return err
	}
	values := append(append([]string{}, setValues...), fleetValues...)
	if err := runInstall(imageTag, values); err != nil {
		return err
	}
	// The chart owns celln-system; the nodes and the router block on these
	// objects until they exist, so publishing after the install is safe.
	if err := cellninstall.PublishFleetModelCredential(ctx, k8sClient, f.modelCredentialFile, f.options.Model); err != nil {
		return err
	}
	if err := cellninstall.PrepareFleetTrust(ctx, k8sClient, f.options.Principal); err != nil {
		return err
	}
	fmt.Println("  Fleet plane deployed. Join KVM nodes with: kubectl label node NODE celln.dev/kvm=true")
	fmt.Printf("  Waiting up to %s for the first node to admit the package and publish the starter configuration...\n", f.wait)
	configuration := filepath.Join(f.outputDir, "configuration")
	deadline := time.Now().Add(f.wait)
	wait := func(what string, ready func() (bool, error)) error {
		for {
			done, err := ready()
			if err != nil || done {
				return err
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("%s did not happen within %s; label a KVM node, check the celln-node prepare logs in celln-system, then rerun this command", what, f.wait)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
			}
		}
	}
	if err := wait("publishing the starter configuration", func() (bool, error) {
		return cellninstall.ReadFleetConfiguration(ctx, k8sClient, configuration)
	}); err != nil {
		return err
	}
	if err := wait("the controller rollout", func() (bool, error) {
		return cellninstall.ControllerRolledOut(ctx, k8sClient, helmNamespace)
	}); err != nil {
		return err
	}
	clusterID, err := cellninstall.ClusterIdentity(ctx, k8sClient)
	if err != nil {
		return err
	}
	platform := cellninstall.PlatformOptions{Namespace: namespace, ConfigurationDir: configuration, OutputDir: filepath.Join(f.outputDir, "installation"), Scope: f.options.Scope, ClusterID: clusterID, PackageHash: f.options.PackageHash, Principal: f.options.Principal, ControllerNamespace: helmNamespace, Authorise: f.authorise}
	if err := cellninstall.InstallPlatform(ctx, k8sClient, platform); err != nil {
		return err
	}
	o := cellninstall.Options{Namespace: namespace, OutputDir: platform.OutputDir, OwnerTarget: cellninstall.ManagedRouterURL, Scope: f.options.Scope, ControllerNamespace: helmNamespace, PackageHash: f.options.PackageHash}
	wiring, err := cellninstall.ConfigureFleet(ctx, k8sClient, o)
	if err != nil {
		return err
	}
	if err := runInstall(imageTag, append(values, wiring...)); err != nil {
		return err
	}
	if f.authorise == cellnplatform.AuthoriseLabeled {
		fmt.Printf("  Enabled enduring Celln runs on fleet %q for namespaces labeled %s=%s; %s is labeled and carries the wrapper objects. No run submitted.\n", f.options.Scope, cellninstall.ScopeLabel, f.options.Scope, namespace)
	} else {
		fmt.Printf("  Enabled enduring Celln runs on fleet %q for every namespace except the system exclusions and namespaces labeled %s; %s carries the wrapper objects and any other namespace gets them on first use. No run submitted.\n", f.options.Scope, cellnplatform.ExcludedLabel, namespace)
	}
	return nil
}
