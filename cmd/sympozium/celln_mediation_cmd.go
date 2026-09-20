package main

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/sympozium-ai/sympozium/internal/cellninstall"
)

func newCellnMediationCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "celln-mediation", Short: "Prepare the operator trust for mediated Celln model access (chart value celln.mediation)"}
	var (
		clusterID, systemNamespace, release, valuesOut string
		gatewayHosts, receiverHosts                    []string
		validity                                       time.Duration
	)
	bootstrap := &cobra.Command{
		Use: "bootstrap", Args: cobra.NoArgs, SilenceUsage: true,
		Short: "Mint the issuer key, JWKS, transport tokens and private CA once, and publish them as Secrets/ConfigMaps",
		Long: "Creates celln-mediation-controller and celln-mediation-gateway (Secrets, control-plane namespace), celln-mediation-node (Secret, celln-system) and celln-mediation-trust (public ConfigMap, both namespaces). " +
			"Private keys and tokens exist only in those Secrets; the CA key is discarded. An existing installation is verified and never replaced. " +
			"Prints the credential-free chart values to enable celln.mediation; modelGateway.image, .egress and .database remain operator inputs.",
		RunE: func(cmd *cobra.Command, args []string) error {
			defaultGateway, defaultReceiver := cellninstall.DefaultMediationHosts(release, systemNamespace)
			if len(gatewayHosts) == 0 {
				gatewayHosts = defaultGateway
			}
			if len(receiverHosts) == 0 {
				receiverHosts = defaultReceiver
			}
			trust, err := cellninstall.PrepareMediationTrust(cmd.Context(), k8sClient, cellninstall.MediationOptions{ClusterID: clusterID, SystemNamespace: systemNamespace, GatewayHosts: gatewayHosts, ReceiverHosts: receiverHosts, Validity: validity})
			if err != nil {
				return err
			}
			state := "verified existing"
			if trust.Created {
				state = "published new"
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "%s mediation trust (issuer key id %s); certificates answer for gateway %v and receiver %v\n", state, trust.KeyID, gatewayHosts, receiverHosts)
			if valuesOut != "" {
				return os.WriteFile(valuesOut, []byte(trust.Values()), 0o644)
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), trust.Values())
			return err
		},
	}
	bootstrap.Flags().StringVar(&clusterID, "cluster-id", "", "Operator-chosen cluster identity bound into every decision (required)")
	bootstrap.Flags().StringVar(&systemNamespace, "system-namespace", "sympozium-system", "Control-plane namespace of the Sympozium release")
	bootstrap.Flags().StringVar(&release, "release-fullname", "sympozium", "Chart full name; the gateway Service is <fullname>-model-gateway")
	bootstrap.Flags().StringSliceVar(&gatewayHosts, "gateway-host", nil, "Gateway certificate names (default: the chart's gateway Service)")
	bootstrap.Flags().StringSliceVar(&receiverHosts, "receiver-host", nil, "Receiver certificate names or IPs; must include the host of celln.mediation.receiver.url (default: the chart's celln-scoped-receiver Service)")
	bootstrap.Flags().DurationVar(&validity, "validity", 365*24*time.Hour, "Lifetime of the CA and both certificates")
	bootstrap.Flags().StringVar(&valuesOut, "values-out", "", "Write the chart values to this file instead of stdout")
	_ = bootstrap.MarkFlagRequired("cluster-id")
	cmd.AddCommand(bootstrap)
	return cmd
}
