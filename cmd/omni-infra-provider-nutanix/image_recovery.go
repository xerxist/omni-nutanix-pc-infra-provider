// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	"github.com/siderolabs/omni/client/pkg/client"
	omniclient "github.com/siderolabs/omni/client/pkg/client/omni"
	"github.com/spf13/cobra"

	"github.com/xerxist/omni-nutanix-pc-infra-provider/internal/pkg/config"
	"github.com/xerxist/omni-nutanix-pc-infra-provider/internal/pkg/provider"
	nclient "github.com/xerxist/omni-nutanix-pc-infra-provider/internal/pkg/provider/client"
	"github.com/xerxist/omni-nutanix-pc-infra-provider/internal/pkg/provider/meta"
)

func newImageImportRecoveryCommand() *cobra.Command {
	var input provider.ImageRecoveryInput
	var runtime runtimeConfig
	var apply bool
	cmd := &cobra.Command{
		Use: "image-import-recovery", Short: "Review or approve one successor for a deleted, successfully imported image",
		Long:         "GET-only by default. --apply creates an unsubmitted successor journal and one immutable provider-scoped recovery approval, not an image or VM. The old submitted journal is never reset or deleted. Only future user-created Omni requests can start the fresh shared attempt. Requires zero provider requests/machines, successful exact task-family evidence and retired-image HTTP 404. TLS verification is mandatory.",
		SilenceUsage: true, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := input.Validate(); err != nil {
				return err
			}
			if err := validateRecoveryOmniEndpoint(runtime.omniAPIEndpoint); err != nil {
				return err
			}
			if err := applyRuntimeEnvironment(cmd, &runtime); err != nil {
				return fmt.Errorf("recovery runtime configuration invalid; details suppressed")
			}
			if runtime.insecureSkipVerify || runtime.registrationOnly {
				return fmt.Errorf("recovery requires verified Omni TLS and cannot run in registration-only mode")
			}
			if runtime.omniAPIEndpoint == "" || runtime.serviceAccountKey == "" {
				return fmt.Errorf("recovery requires the Omni endpoint and provider-scoped service account")
			}
			cfg, err := config.Load(runtime.configFile)
			if err != nil || cfg.Validate() != nil {
				return fmt.Errorf("recovery Prism configuration invalid; details suppressed")
			}
			if cfg.Nutanix.InsecureSkipVerify || cfg.Nutanix.VMMVersion != "" && cfg.Nutanix.VMMVersion != "v4.0" || cfg.Nutanix.PrismVersion != "" && cfg.Nutanix.PrismVersion != "v4.0" {
				return fmt.Errorf("recovery requires verified Prism TLS and GA v4.0 VMM/task APIs")
			}
			nc, err := nclient.NewV4(nclient.V4Options{Endpoint: cfg.Nutanix.Endpoint, Username: cfg.Nutanix.Username, Password: cfg.Nutanix.Password, APIKey: cfg.Nutanix.APIKey, CAFile: cfg.Nutanix.CAFile})
			if err != nil {
				return fmt.Errorf("recovery Prism client configuration invalid; details suppressed")
			}
			c, err := client.New(runtime.omniAPIEndpoint, client.WithServiceAccount(runtime.serviceAccountKey), client.WithOmniClientOptions(omniclient.WithProviderID(meta.ProviderID)))
			if err != nil {
				return fmt.Errorf("recovery Omni client unavailable; details suppressed")
			}
			defer c.Close() //nolint:errcheck
			ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
			defer cancel()
			summary, err := provider.ApproveImageImportRecovery(ctx, c.Omni().State(), nc, input, apply)
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(summary)
		},
	}
	cmd.Flags().StringVar(&input.CacheID, "cache-id", "", "Exact original ImageImport cache ID")
	cmd.Flags().StringVar(&input.JournalSHA256, "expected-journal-sha256", "", "SHA256 of the original journal's protobuf spec bytes")
	cmd.Flags().StringVar(&input.JournalVersion, "expected-journal-version", "", "Exact original journal resource version")
	cmd.Flags().StringVar(&input.JournalCreated, "expected-journal-created", "", "Exact original journal creation timestamp in canonical UTC RFC3339Nano")
	cmd.Flags().StringVar(&input.RetiredImageUUID, "retired-image-uuid", "", "Operator-confirmed deleted image UUID")
	cmd.Flags().StringArrayVar(&input.TaskIDs, "task-id", nil, "Exact successful task ID; repeat for every reported family member")
	cmd.Flags().StringVar(&input.AttemptUUID, "attempt-uuid", "", "Explicit fresh random v4 UUID; reuse the same UUID when reviewing/repeating this command")
	cmd.Flags().BoolVar(&apply, "apply", false, "Create the immutable approval after fresh checks; never POST an image or VM")
	cmd.Flags().StringVar(&runtime.omniAPIEndpoint, "omni-api-endpoint", environmentDefault("OMNI_ENDPOINT", ""), "Verified Omni HTTPS API endpoint")
	cmd.Flags().StringVar(&runtime.serviceAccountKey, "omni-service-account-key", "", "Provider-scoped key; prefer OMNI_SERVICE_ACCOUNT_KEY_FILE")
	cmd.Flags().StringVar(&runtime.configFile, "config-file", environmentDefault("PROVIDER_CONFIG_FILE", ""), "Prism config file; normal NUTANIX_* mounted-secret settings also apply")
	return cmd
}

func validateRecoveryOmniEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Path != "" && u.Path != "/" {
		return fmt.Errorf("recovery requires an Omni HTTPS origin without credentials, query or fragment; details suppressed")
	}
	return nil
}
