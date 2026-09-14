package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"github.com/sympozium-ai/sympozium/internal/cellninstall"
)

// cellnFleetFlags configure `sympozium install --celln-fleet`: one reviewed
// package, one scope, and every node labeled celln.dev/kvm=true joins.
type cellnFleetFlags struct {
	enabled             bool
	options             cellninstall.FleetOptions
	modelCredentialFile string
	outputDir           string
	wait                time.Duration
}

func (f *cellnFleetFlags) register(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&f.enabled, "celln-fleet", false, "Run the native Celln plane as a per-node fleet: every node labeled celln.dev/kvm=true prepares the reviewed starter package and serves enduring parents (requires the --celln-fleet-* inputs and --celln-native-approve-starter-tools)")
	cmd.Flags().StringVar(&f.options.Scope, "celln-fleet-scope", "", "Stable installation identity; node state lives at /var/lib/sympozium-celln/<scope>")
	cmd.Flags().StringVar(&f.options.PackageImage, "celln-fleet-package-image", "", "Digest-pinned OCI image (repository@sha256:...) carrying the reviewed 'celln starter-package' output at /package")
	cmd.Flags().StringVar(&f.options.PackageHash, "celln-fleet-package-hash", "", "Exact operator-approved package BLAKE3 identity from 'celln starter-inspect'")
	cmd.Flags().StringVar(&f.options.Publisher, "celln-fleet-publisher", "", "Explicitly approved publisher key of the package from 'celln starter-inspect'")
	cmd.Flags().StringVar(&f.options.Principal, "celln-fleet-principal", "sympozium:celln", "Parent principal every fleet owner authenticates")
	cmd.Flags().StringVar(&f.options.ModelCredentialPath, "celln-fleet-model-credential-path", "/etc/celln-native/model-token", "Absolute path inside every dispatcher where the model credential Secret is mounted; recorded in the model profile")
	cmd.Flags().StringVar(&f.modelCredentialFile, "celln-fleet-model-credential-file", "", "Local file holding the model provider credential to publish once as a Secret in celln-system (omit to keep an existing Secret)")
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
	if err := cellninstall.PublishFleetModelCredential(ctx, k8sClient, f.modelCredentialFile); err != nil {
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
	platform := cellninstall.PlatformOptions{Namespace: namespace, ConfigurationDir: configuration, OutputDir: filepath.Join(f.outputDir, "installation"), Scope: f.options.Scope, ClusterID: clusterID, PackageHash: f.options.PackageHash, Principal: f.options.Principal, ControllerNamespace: helmNamespace}
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
	fmt.Printf("  Enabled enduring Celln runs on fleet %q; namespace %s is labeled %s=%s and carries the wrapper objects. Authorise more namespaces with that label plus an AgentRuntime referencing profile celln-native-%s. No run submitted.\n", f.options.Scope, namespace, cellninstall.ScopeLabel, f.options.Scope, f.options.Scope)
	return nil
}
