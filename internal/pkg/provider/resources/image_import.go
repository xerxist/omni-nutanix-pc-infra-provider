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

// ImageImport stores a bounded JSON journal in a standard protobuf string carrier.
// It is shared across requests and retained independently of VM teardown.
type ImageImport = typed.Resource[ImageImportSpec, ImageImportExtension]
type ImageImportSpec = protobuf.ResourceSpec[wrapperspb.StringValue, *wrapperspb.StringValue]
type ImageImportExtension struct{}

func NewImageImport(id string) *ImageImport {
	return typed.NewResource[ImageImportSpec, ImageImportExtension](
		resource.NewMetadata(infra.ResourceNamespace(providermeta.ProviderID), infra.ResourceType("ImageImport", providermeta.ProviderID), id, resource.VersionUndefined),
		protobuf.NewResourceSpec(&wrapperspb.StringValue{}))
}

func (ImageImportExtension) ResourceDefinition() meta.ResourceDefinitionSpec {
	return meta.ResourceDefinitionSpec{Type: infra.ResourceType("ImageImport", providermeta.ProviderID), DefaultNamespace: infra.ResourceNamespace(providermeta.ProviderID)}
}
