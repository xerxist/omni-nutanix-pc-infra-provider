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

// ImageImportReplacement records one immutable successor of a completed, deleted
// cache. Its ID is the parent cache ID; neither parent nor marker is ever reset.
type ImageImportReplacement = typed.Resource[ImageImportSpec, ImageImportReplacementExtension]
type ImageImportReplacementExtension struct{}

func NewImageImportReplacement(id string) *ImageImportReplacement {
	return typed.NewResource[ImageImportSpec, ImageImportReplacementExtension](
		resource.NewMetadata(infra.ResourceNamespace(providermeta.ProviderID), infra.ResourceType("ImageImportReplacement", providermeta.ProviderID), id, resource.VersionUndefined),
		protobuf.NewResourceSpec(&wrapperspb.StringValue{}))
}

func (ImageImportReplacementExtension) ResourceDefinition() meta.ResourceDefinitionSpec {
	return meta.ResourceDefinitionSpec{Type: infra.ResourceType("ImageImportReplacement", providermeta.ProviderID), DefaultNamespace: infra.ResourceNamespace(providermeta.ProviderID)}
}
