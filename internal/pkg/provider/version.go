// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

import (
	"context"
	"fmt"
	"regexp"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/state"
	infrares "github.com/siderolabs/omni/client/pkg/omni/resources/infra"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/reflect/protoreflect"
)

const providerVersionField protowire.Number = 5

var publicVersion = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)

// WithRegistrationVersion stamps only this provider's registration writes,
// including the pinned SDK's replacement of the status spec during startup.
// All lifecycle resources, other providers and state reads pass through unchanged.
func WithRegistrationVersion(st state.State, id, version string) (state.State, error) {
	if st == nil || id == "" || !publicVersion.MatchString(version) {
		return nil, fmt.Errorf("provider state, ID and a valid public build version are required")
	}
	// Stamp inside optimistic-update callbacks, before the underlying State's
	// equality check. Stamping only primitive writes misses unchanged metadata
	// on existing providers because UpdateWithConflicts returns without Update.
	return &versionedRegistrationState{State: st, id: id, version: version}, nil
}

type versionedRegistrationState struct {
	state.State
	id, version string
}

func (s *versionedRegistrationState) stamp(res resource.Resource) (resource.Resource, error) {
	if !s.isRegistration(res) {
		return res, nil
	}
	stamped := res.DeepCopy()
	if err := s.stampInPlace(stamped); err != nil {
		return nil, err
	}
	return stamped, nil
}

func (s *versionedRegistrationState) isRegistration(res resource.Resource) bool {
	expected := infrares.NewProviderStatus(s.id).Metadata()
	md := res.Metadata()
	return md.Type() == expected.Type() && md.Namespace() == expected.Namespace() && md.ID() == s.id
}

func (s *versionedRegistrationState) stampInPlace(res resource.Resource) error {
	if !s.isRegistration(res) {
		return nil
	}
	status, ok := res.(*infrares.ProviderStatus)
	if !ok {
		return fmt.Errorf("unexpected provider registration resource type")
	}
	return setRegistrationVersion(status.TypedSpec().Value.ProtoReflect(), s.version)
}

func (s *versionedRegistrationState) Create(ctx context.Context, res resource.Resource, opts ...state.CreateOption) error {
	stamped, err := s.stamp(res)
	if err != nil {
		return err
	}
	if err := s.State.Create(ctx, stamped, opts...); err != nil {
		return err
	}
	// Remote State writes update the caller's resource metadata (resource version,
	// owner, timestamps). Preserve that contract while leaving its spec untouched.
	*res.Metadata() = stamped.Metadata().Copy()
	return nil
}

func (s *versionedRegistrationState) Update(ctx context.Context, res resource.Resource, opts ...state.UpdateOption) error {
	stamped, err := s.stamp(res)
	if err != nil {
		return err
	}
	if err := s.State.Update(ctx, stamped, opts...); err != nil {
		return err
	}
	*res.Metadata() = stamped.Metadata().Copy()
	return nil
}

func (s *versionedRegistrationState) versionedUpdater(update state.UpdaterFunc) state.UpdaterFunc {
	return func(res resource.Resource) error {
		if err := update(res); err != nil {
			return err
		}
		// This is the updater's private copy: include the version before the
		// equality check decides whether to write, leaving reads untouched.
		return s.stampInPlace(res)
	}
}

func (s *versionedRegistrationState) UpdateWithConflicts(ctx context.Context, ptr resource.Pointer, update state.UpdaterFunc, opts ...state.UpdateOption) (resource.Resource, error) {
	return s.State.UpdateWithConflicts(ctx, ptr, s.versionedUpdater(update), opts...)
}

func (s *versionedRegistrationState) Modify(ctx context.Context, empty resource.Resource, update func(resource.Resource) error, opts ...state.UpdateOption) error {
	_, err := s.ModifyWithResult(ctx, empty, update, opts...)
	return err
}

func (s *versionedRegistrationState) ModifyWithResult(ctx context.Context, empty resource.Resource, update func(resource.Resource) error, opts ...state.UpdateOption) (resource.Resource, error) {
	return s.State.ModifyWithResult(ctx, empty, s.versionedUpdater(update), opts...)
}

func setRegistrationVersion(message protoreflect.Message, version string) error {
	// Omni's official InfraProviderStatusSpec uses string field 5 for version:
	// https://github.com/siderolabs/omni/blob/v1.12.4/client/api/omni/specs/infra.proto
	// Our v1.7.1 SDK predates it. Use protobuf's forward-compatible unknown fields
	// rather than upgrading the provisioning SDK for a display-only change.
	field := message.Descriptor().Fields().ByNumber(protoreflect.FieldNumber(providerVersionField))
	if field != nil {
		if field.Name() != "version" || field.Kind() != protoreflect.StringKind || field.IsList() {
			return fmt.Errorf("unsupported provider version field definition")
		}
	}
	unknown := message.GetUnknown()
	updated := make([]byte, 0, len(unknown)+len(version)+4)
	for len(unknown) > 0 {
		number, kind, tagLength := protowire.ConsumeTag(unknown)
		if tagLength < 0 {
			return fmt.Errorf("malformed provider registration extension fields")
		}
		valueLength := protowire.ConsumeFieldValue(number, kind, unknown[tagLength:])
		if valueLength < 0 {
			return fmt.Errorf("malformed provider registration extension fields")
		}
		if number != providerVersionField {
			updated = append(updated, unknown[:tagLength+valueLength]...)
		} else if kind != protowire.BytesType {
			return fmt.Errorf("invalid provider registration version wire type")
		}
		unknown = unknown[tagLength+valueLength:]
	}
	if field != nil {
		message.Set(field, protoreflect.ValueOfString(version))
	} else {
		updated = protowire.AppendTag(updated, providerVersionField, protowire.BytesType)
		updated = protowire.AppendString(updated, version)
	}
	message.SetUnknown(updated)
	return nil
}
