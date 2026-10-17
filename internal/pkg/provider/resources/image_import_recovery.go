// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package resources

import (
	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/resource/meta"
	"github.com/cosi-project/runtime/pkg/resource/protobuf"
	"github.com/cosi-project/runtime/pkg/resource/typed"
	"github.com/siderolabs/omni/client/pkg/infra"
	"google.golang.org/protobuf/types/known/wrapperspb"

	providermeta "github.com/xerxist/omni-nutanix-pc-infra-provider/internal/pkg/provider/meta"
)

// ImageImportRecovery is an immutable, explicitly approved successor intent.
// Its ID is the original cache ID; it neither overwrites nor retires that journal.
type ImageImportRecovery = typed.Resource[ImageImportSpec, ImageImportRecoveryExtension]
type ImageImportRecoveryExtension struct{}

func NewImageImportRecovery(id string) *ImageImportRecovery {
	return typed.NewResource[ImageImportSpec, ImageImportRecoveryExtension](
		resource.NewMetadata(infra.ResourceNamespace(providermeta.ProviderID), infra.ResourceType("ImageImportRecovery", providermeta.ProviderID), id, resource.VersionUndefined),
		protobuf.NewResourceSpec(&wrapperspb.StringValue{}))
}

func (ImageImportRecoveryExtension) ResourceDefinition() meta.ResourceDefinitionSpec {
	return meta.ResourceDefinitionSpec{Type: infra.ResourceType("ImageImportRecovery", providermeta.ProviderID), DefaultNamespace: infra.ResourceNamespace(providermeta.ProviderID)}
}
