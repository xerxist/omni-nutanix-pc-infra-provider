// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package config contains configuration types and validation for the nutanix provider.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config describes the nutanix provider configuration.
type Config struct {
	Nutanix     NutanixConfig     `yaml:"nutanix"`
	Guest       GuestConfig       `yaml:"guest,omitempty"`
	ImageImport ImageImportConfig `yaml:"imageImport,omitempty"`
}

// ImageImportConfig is a provider-wide mutation gate and independent factory trust.
type ImageImportConfig struct {
	Enabled         bool   `yaml:"enabled,omitempty"`
	FactoryEndpoint string `yaml:"factoryEndpoint,omitempty"`
	CAFile          string `yaml:"caFile,omitempty"`
}

// GuestConfig configures optional Talos bootstrap trust, independently of PC TLS.
type GuestConfig struct {
	CAFile string `yaml:"caFile,omitempty"`
}

// NutanixConfig holds Prism Central credentials and namespace API versions.
type NutanixConfig struct {
	Endpoint           string `yaml:"endpoint"`
	Username           string `yaml:"username"`
	Password           string `yaml:"password"`
	APIKey             string `yaml:"apiKey,omitempty"`
	InsecureSkipVerify bool   `yaml:"insecureSkipVerify,omitempty"`
	CAFile             string `yaml:"caFile,omitempty"`
	VMMVersion         string `yaml:"vmmVersion,omitempty"`
	NetworkingVersion  string `yaml:"networkingVersion,omitempty"`
	ClusterVersion     string `yaml:"clusterVersion,omitempty"`
	PrismVersion       string `yaml:"prismVersion,omitempty"`
}

// Load reads configuration from the given YAML file (if non-empty) and applies
// environment-variable overrides. Env vars take precedence.
func Load(path string) (*Config, error) {
	cfg := &Config{}

	if path != "" {
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("failed to open config file %q: %w", path, err)
		}

		defer f.Close()

		decoder := yaml.NewDecoder(f)
		decoder.KnownFields(true)
		if err := decoder.Decode(cfg); err != nil {
			return nil, fmt.Errorf("failed to parse config file %q: %w", path, err)
		}
	}

	if v := os.Getenv("NUTANIX_ENDPOINT"); v != "" {
		cfg.Nutanix.Endpoint = v
	}
	if v := os.Getenv("TALOS_GUEST_CA_FILE"); v != "" {
		cfg.Guest.CAFile = v
	}
	if v := os.Getenv("IMAGE_IMPORT_ENABLED"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("invalid IMAGE_IMPORT_ENABLED")
		}
		cfg.ImageImport.Enabled = b
	}
	if v := os.Getenv("IMAGE_FACTORY_ENDPOINT"); v != "" {
		cfg.ImageImport.FactoryEndpoint = v
	}
	if v := os.Getenv("IMAGE_FACTORY_CA_FILE"); v != "" {
		cfg.ImageImport.CAFile = v
	}

	for name, target := range map[string]*string{
		"NUTANIX_USERNAME": &cfg.Nutanix.Username,
		"NUTANIX_PASSWORD": &cfg.Nutanix.Password,
		"NUTANIX_API_KEY":  &cfg.Nutanix.APIKey,
	} {
		value, present, err := ReadSecretEnvironment(name)
		if err != nil {
			return nil, err
		}
		if present {
			*target = value
		}
	}
	for key, target := range map[string]*string{
		"NUTANIX_CA_FILE":            &cfg.Nutanix.CAFile,
		"NUTANIX_VMM_VERSION":        &cfg.Nutanix.VMMVersion,
		"NUTANIX_NETWORKING_VERSION": &cfg.Nutanix.NetworkingVersion,
		"NUTANIX_CLUSTER_VERSION":    &cfg.Nutanix.ClusterVersion,
		"NUTANIX_PRISM_VERSION":      &cfg.Nutanix.PrismVersion,
	} {
		if v := os.Getenv(key); v != "" {
			*target = v
		}
	}

	if v := os.Getenv("NUTANIX_INSECURE_SKIP_VERIFY"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("invalid NUTANIX_INSECURE_SKIP_VERIFY %q: %w", v, err)
		}

		cfg.Nutanix.InsecureSkipVerify = b
	}

	return cfg, nil
}

// Validate checks that all required fields are set.
func (c *Config) Validate() error {
	if c.Nutanix.Endpoint == "" {
		return fmt.Errorf("nutanix.endpoint is required")
	}

	if c.Nutanix.APIKey != "" {
		if c.Nutanix.Username != "" || c.Nutanix.Password != "" {
			return fmt.Errorf("configure either nutanix.apiKey or username/password, not both")
		}
		if strings.TrimSpace(c.Nutanix.APIKey) == "" || strings.ContainsAny(c.Nutanix.APIKey, "\r\n") {
			return fmt.Errorf("nutanix.apiKey must be a nonempty single-line key")
		}
		return nil
	}

	if c.Nutanix.Username == "" {
		return fmt.Errorf("nutanix.username is required")
	}

	if c.Nutanix.Password == "" {
		return fmt.Errorf("nutanix.password is required")
	}

	return nil
}
