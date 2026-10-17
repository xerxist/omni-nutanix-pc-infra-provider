// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"context"
	"fmt"

	"github.com/cosi-project/runtime/pkg/controller/runtime"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/siderolabs/omni/client/pkg/infra"
	"github.com/siderolabs/omni/client/pkg/infra/controllers"
	infrares "github.com/siderolabs/omni/client/pkg/omni/resources/infra"
	"go.uber.org/zap"
)

const ModeAnnotation = "nutanix.infra.sidero.dev/runtime-mode"
const RegistrationOnlyMode = "registration-only"
const ActiveMode = "active"

// PublishActiveRegistration replaces stale pause metadata before the SDK starts
// its active runtime. It does not process requests or alter their finalizers.
func PublishActiveRegistration(ctx context.Context, st state.State, id string, cfg infra.ProviderConfig) error {
	return publishRegistration(ctx, st, id, cfg, ActiveMode)
}

func publishRegistration(ctx context.Context, st state.State, id string, cfg infra.ProviderConfig, mode string) error {
	if id == "" {
		return fmt.Errorf("provider ID is required")
	}
	status := infrares.NewProviderStatus(id)
	status.Metadata().Annotations().Set(ModeAnnotation, mode)
	v := status.TypedSpec().Value
	v.Name, v.Description, v.Schema, v.Icon = cfg.Name, cfg.Description, cfg.Schema, cfg.Icon
	if err := st.Create(ctx, status); err != nil {
		if !state.IsConflictError(err) {
			return err
		}
		if _, err := safe.StateUpdateWithConflicts(ctx, st, status.Metadata(), func(res *infrares.ProviderStatus) error {
			res.TypedSpec().Value = status.TypedSpec().Value
			res.Metadata().Annotations().Set(ModeAnnotation, mode)
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// RunRegistrationOnly publishes provider metadata and heartbeat without ever
// constructing a Prism client or registering a provisioning/teardown controller.
// Existing requests, lifecycle state, patches and finalizers are left untouched.
func RunRegistrationOnly(ctx context.Context, logger *zap.Logger, st state.State, id string, cfg infra.ProviderConfig) error {
	if id == "" {
		return fmt.Errorf("provider ID is required")
	}
	rt, err := runtime.NewRuntime(st, logger)
	if err != nil {
		return err
	}
	health, err := controllers.NewProviderHealthStatusController(id, controllers.ProviderHealthStatusOptions{})
	if err != nil {
		return err
	}
	if err := rt.RegisterController(health); err != nil {
		return err
	}
	if err := publishRegistration(ctx, st, id, cfg, RegistrationOnlyMode); err != nil {
		return err
	}
	logger.Warn("registration-only mode: VM provisioning and teardown are disabled", zap.String("provider_id", id))
	return rt.Run(ctx)
}
