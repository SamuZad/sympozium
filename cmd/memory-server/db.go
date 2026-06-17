package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/rds/auth"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// newPool creates a pgxpool.Pool honouring the configured database auth
// mode. Supported modes:
//
//   "" / "none" / "password" — DATABASE_URL is used verbatim. Covers
//                              password auth, mTLS, peer/trust, ~/.pgpass,
//                              Cloud SQL Auth Proxy, GCP IAM via the proxy
//                              sidecar, and anything else libpq handles
//                              without help.
//   "rds-iam"                — every new connection re-mints an AWS RDS
//                              IAM auth token via the default AWS
//                              credential chain (IRSA / instance profile
//                              / env / shared config). Required because
//                              IAM tokens expire after 15 minutes; a
//                              static DATABASE_URL would die on the first
//                              pool reconnect.
func newPool(ctx context.Context, cfg *Config) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}

	switch strings.ToLower(cfg.DBAuthMode) {
	case "", "none", "password":
		// Nothing to do.
	case "rds-iam":
		if err := installRDSIAMHook(ctx, cfg, poolCfg); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported MEMORY_DB_AUTH=%q (expected one of: password, rds-iam)", cfg.DBAuthMode)
	}

	return pgxpool.NewWithConfig(ctx, poolCfg)
}

// installRDSIAMHook wires a BeforeConnect callback that generates a fresh
// IAM token on every new connection. The token is injected as the password
// on the per-connection ConnConfig — the pool config itself is untouched.
func installRDSIAMHook(ctx context.Context, cfg *Config, poolCfg *pgxpool.Config) error {
	var opts []func(*config.LoadOptions) error
	if cfg.AWSRegion != "" {
		opts = append(opts, config.WithRegion(cfg.AWSRegion))
	}
	awsCfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return fmt.Errorf("load AWS config: %w", err)
	}
	region := awsCfg.Region
	if region == "" {
		return fmt.Errorf("rds-iam auth requires an AWS region (set AWS_REGION or MEMORY_AWS_REGION)")
	}

	endpoint := fmt.Sprintf("%s:%d", poolCfg.ConnConfig.Host, poolCfg.ConnConfig.Port)
	dbUser := poolCfg.ConnConfig.User
	if dbUser == "" {
		return fmt.Errorf("rds-iam auth requires a user in DATABASE_URL")
	}

	creds := awsCfg.Credentials
	poolCfg.BeforeConnect = func(ctx context.Context, cc *pgx.ConnConfig) error {
		token, err := auth.BuildAuthToken(ctx, endpoint, region, dbUser, creds)
		if err != nil {
			return fmt.Errorf("build RDS IAM auth token: %w", err)
		}
		cc.Password = token
		return nil
	}
	return nil
}
