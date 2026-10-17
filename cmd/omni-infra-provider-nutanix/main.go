// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package main is the entry point for the Nutanix Omni infra provider.
package main

import (
	"context"
	_ "embed"
	"encoding/base64"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/siderolabs/omni/client/pkg/client"
	omniclient "github.com/siderolabs/omni/client/pkg/client/omni"
	"github.com/siderolabs/omni/client/pkg/infra"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/xerxist/omni-nutanix-pc-infra-provider/internal/pkg/buildinfo"
	"github.com/xerxist/omni-nutanix-pc-infra-provider/internal/pkg/config"
	"github.com/xerxist/omni-nutanix-pc-infra-provider/internal/pkg/provider"
	nclient "github.com/xerxist/omni-nutanix-pc-infra-provider/internal/pkg/provider/client"
	"github.com/xerxist/omni-nutanix-pc-infra-provider/internal/pkg/provider/meta"
)

//go:embed data/schema.json
var schema string

//go:embed data/icon.svg
var icon []byte

var rootCmd = &cobra.Command{
	Use:          "provider",
	Short:        "Nutanix Omni infrastructure provider",
	Long:         `Connects to Omni and manages Talos VMs through Nutanix Prism Central GA v4 APIs.`,
	Version:      buildinfo.Version,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if err := applyRuntimeEnvironment(cmd, &cfg); err != nil {
			return err
		}
		logger, err := zap.NewProductionConfig().Build(zap.AddStacktrace(zapcore.ErrorLevel))
		if err != nil {
			return fmt.Errorf("failed to create logger: %w", err)
		}

		if cfg.omniAPIEndpoint == "" {
			return fmt.Errorf("--omni-api-endpoint flag is not set (or OMNI_ENDPOINT env var)")
		}
		if _, err := compileProviderSchema(schema); err != nil {
			return fmt.Errorf("invalid embedded provider schema: %w", err)
		}
		providerConfig := infra.ProviderConfig{
			Name: cfg.providerName, Description: cfg.providerDescription,
			Icon: base64.RawStdEncoding.EncodeToString(icon), Schema: schema,
		}
		clientOptions := []client.Option{
			client.WithInsecureSkipTLSVerify(cfg.insecureSkipVerify),
		}
		if cfg.serviceAccountKey != "" {
			clientOptions = append(clientOptions, client.WithServiceAccount(cfg.serviceAccountKey))
		}
		if cfg.registrationOnly {
			// Do not load Prism credentials, construct its client, or instantiate
			// a provisioner in this mode. A fresh heartbeat does not mean VM work
			// is enabled or that Prism health has been checked.
			clientOptions = append(clientOptions, client.WithOmniClientOptions(omniclient.WithProviderID(meta.ProviderID)))
			c, err := client.New(cfg.omniAPIEndpoint, clientOptions...)
			if err != nil {
				return fmt.Errorf("construct registration-only Omni client: %w", err)
			}
			defer c.Close() //nolint:errcheck
			st, err := provider.WithRegistrationVersion(c.Omni().State(), meta.ProviderID, buildinfo.Version)
			if err != nil {
				return err
			}
			return provider.RunRegistrationOnly(cmd.Context(), logger, st, meta.ProviderID, providerConfig)
		}

		providerCfg, err := config.Load(cfg.configFile)
		if err != nil {
			return err
		}

		if err = providerCfg.Validate(); err != nil {
			return fmt.Errorf("invalid nutanix configuration: %w", err)
		}

		nc, err := nclient.NewV4(nclient.V4Options{
			Endpoint:           providerCfg.Nutanix.Endpoint,
			Username:           providerCfg.Nutanix.Username,
			Password:           providerCfg.Nutanix.Password,
			APIKey:             providerCfg.Nutanix.APIKey,
			InsecureSkipVerify: providerCfg.Nutanix.InsecureSkipVerify,
			CAFile:             providerCfg.Nutanix.CAFile,
			VMMVersion:         providerCfg.Nutanix.VMMVersion,
			NetworkingVersion:  providerCfg.Nutanix.NetworkingVersion,
			ClusterVersion:     providerCfg.Nutanix.ClusterVersion,
			PrismVersion:       providerCfg.Nutanix.PrismVersion,
		})
		if err != nil {
			return fmt.Errorf("failed to construct Nutanix client: %w", err)
		}

		var guestCA []byte
		if path := providerCfg.Guest.CAFile; path != "" {
			guestCA, err = os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("read guest CA bundle: %w", err)
			}
			if len(guestCA) == 0 {
				return fmt.Errorf("guest CA bundle is empty")
			}
		}
		provisioner, err := provider.NewProvisionerWithGuestCA(nc, guestCA)
		if err != nil {
			return fmt.Errorf("configure guest trust: %w", err)
		}

		clientOptions = append(clientOptions, client.WithOmniClientOptions(omniclient.WithProviderID(meta.ProviderID)))
		c, err := client.New(cfg.omniAPIEndpoint, clientOptions...)
		if err != nil {
			return fmt.Errorf("construct active Omni client: %w", err)
		}
		defer c.Close() //nolint:errcheck
		st, err := provider.WithRegistrationVersion(c.Omni().State(), meta.ProviderID, buildinfo.Version)
		if err != nil {
			return err
		}
		provisioner.ConfigureMigrationQueue(st)
		runOptions := []infra.Option{infra.WithState(st), infra.WithEncodeRequestIDsIntoTokens(), infra.WithHealthCheckFunc(nc.CheckHealth)}
		if providerCfg.ImageImport.Enabled {
			factory, err := provider.NewImageFactory(providerCfg.ImageImport.FactoryEndpoint, providerCfg.ImageImport.CAFile)
			if err != nil {
				return fmt.Errorf("configure Image Factory: %w", err)
			}
			if err := provisioner.ConfigureImageImport(st, factory); err != nil {
				return err
			}
			runOptions = append(runOptions, infra.WithImageFactoryClient(factory))
		}
		ip, err := infra.NewProvider(meta.ProviderID, provisioner, providerConfig)
		if err != nil {
			return fmt.Errorf("failed to create infra provider: %w", err)
		}
		if err := provider.PublishActiveRegistration(cmd.Context(), st, meta.ProviderID, providerConfig); err != nil {
			return fmt.Errorf("publish active provider metadata: %w", err)
		}

		logger.Info("starting infra provider",
			zap.String("provider_id", meta.ProviderID),
			zap.String("provider_version", buildinfo.Version),
			zap.String("nutanix_endpoint", providerCfg.Nutanix.Endpoint),
			zap.String("omni_endpoint", cfg.omniAPIEndpoint))

		return ip.Run(
			cmd.Context(),
			logger,
			runOptions...,
		)
	},
}

var cfg runtimeConfig

func main() {
	if err := app(); err != nil {
		os.Exit(1)
	}
}

func app() error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGHUP, syscall.SIGTERM)
	defer cancel()

	return rootCmd.ExecuteContext(ctx)
}

func init() {
	rootCmd.AddCommand(newImageImportRecoveryCommand())
	rootCmd.Flags().StringVar(&cfg.omniAPIEndpoint, "omni-api-endpoint", os.Getenv("OMNI_ENDPOINT"),
		"endpoint of the Omni API; defaults to $OMNI_ENDPOINT")
	rootCmd.Flags().StringVar(&meta.ProviderID, "id", environmentDefault("PROVIDER_ID", meta.ProviderID),
		"infra provider id; matches resources via the infra provider label")
	rootCmd.Flags().StringVar(&cfg.serviceAccountKey, "omni-service-account-key", "",
		"Omni provider key; prefer OMNI_SERVICE_ACCOUNT_KEY_FILE or OMNI_SERVICE_ACCOUNT_KEY")
	rootCmd.Flags().StringVar(&cfg.providerName, "provider-name", environmentDefault("PROVIDER_NAME", "Nutanix Prism Central"),
		"provider name as it appears in Omni")
	rootCmd.Flags().StringVar(&cfg.providerDescription, "provider-description", environmentDefault("PROVIDER_DESCRIPTION", "Nutanix infrastructure provider"),
		"provider description as it appears in Omni")
	rootCmd.Flags().BoolVar(&cfg.insecureSkipVerify, "insecure-skip-verify", false,
		"ignore untrusted certs on the Omni side")
	rootCmd.Flags().StringVar(&cfg.configFile, "config-file", os.Getenv("PROVIDER_CONFIG_FILE"),
		"path to the YAML provider config file (NUTANIX_* env vars also accepted)")
	rootCmd.Flags().BoolVar(&cfg.registrationOnly, "registration-only", false,
		"publish schema and heartbeat only; disable all VM provisioning and teardown")
}
