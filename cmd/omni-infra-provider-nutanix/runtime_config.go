// This Source Code Form is subject to the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/xerxist/omni-nutanix-pc-infra-provider/internal/pkg/config"
)

type runtimeConfig struct {
	omniAPIEndpoint     string
	serviceAccountKey   string
	providerName        string
	providerDescription string
	configFile          string
	insecureSkipVerify  bool
	registrationOnly    bool
}

func environmentDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

// CLI flags take precedence over environment settings. Read secrets only at
// execution time so --help never exposes a secret as a flag's default value.
func applyRuntimeEnvironment(cmd *cobra.Command, runtime *runtimeConfig) error {
	if !cmd.Flags().Changed("omni-service-account-key") {
		value, present, err := config.ReadSecretEnvironment("OMNI_SERVICE_ACCOUNT_KEY")
		if err != nil {
			return err
		}
		if present {
			runtime.serviceAccountKey = value
		}
	}
	for _, setting := range []struct {
		environment, flag string
		target            *bool
	}{
		{"OMNI_INSECURE_SKIP_VERIFY", "insecure-skip-verify", &runtime.insecureSkipVerify},
		{"PROVIDER_REGISTRATION_ONLY", "registration-only", &runtime.registrationOnly},
	} {
		if value := os.Getenv(setting.environment); value != "" && !cmd.Flags().Changed(setting.flag) {
			parsed, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("%s must be a boolean", setting.environment)
			}
			*setting.target = parsed
		}
	}
	return nil
}
