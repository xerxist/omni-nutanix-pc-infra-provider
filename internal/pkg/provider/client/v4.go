// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// V4Options configures independent v4 namespace versions on Prism Central.
type V4Options struct {
	Endpoint, Username, Password                                string
	APIKey                                                      string
	InsecureSkipVerify                                          bool
	CAFile                                                      string
	VMMVersion, NetworkingVersion, ClusterVersion, PrismVersion string
}

type v4Client struct {
	base, user, pass, apiKey        string
	vmm, networking, cluster, prism string
	http                            *http.Client
}

// NewV4 uses HTTPS, system trust, bounded requests, and no legacy API fallback.
func NewV4(opts V4Options) (Client, error) {
	base := strings.TrimRight(opts.Endpoint, "/")
	if !strings.Contains(base, "://") {
		base = "https://" + base
	}
	u, err := url.Parse(base)
	if err != nil || u.Hostname() == "" || u.Scheme != "https" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("endpoint must be a Prism Central HTTPS origin (for example https://pc.example.com:9440)")
	}
	if opts.APIKey != "" {
		if opts.Username != "" || opts.Password != "" {
			return nil, fmt.Errorf("configure either a Prism Central API key or username/password, not both")
		}
		if strings.TrimSpace(opts.APIKey) == "" || strings.ContainsAny(opts.APIKey, "\r\n") {
			return nil, fmt.Errorf("Prism Central API key must be a nonempty single-line key")
		}
	} else if opts.Username == "" || opts.Password == "" {
		return nil, fmt.Errorf("Prism Central API key or username and password are required")
	}
	versions := []*string{&opts.VMMVersion, &opts.NetworkingVersion, &opts.ClusterVersion, &opts.PrismVersion}
	for _, version := range versions {
		if *version == "" {
			*version = "v4.0"
		}
		if !regexp.MustCompile(`^v4\.[0-9]+$`).MatchString(*version) {
			return nil, fmt.Errorf("invalid GA v4 API version %q", *version)
		}
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: opts.InsecureSkipVerify} //nolint:gosec
	if opts.CAFile != "" {
		pem, err := os.ReadFile(opts.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read Prism Central CA: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("CA file contains no certificates")
		}
		tlsConfig.RootCAs = pool
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig
	return &v4Client{
		base: base, user: opts.Username, pass: opts.Password, apiKey: opts.APIKey,
		vmm:        "/api/vmm/" + opts.VMMVersion,
		networking: "/api/networking/" + opts.NetworkingVersion + "/config",
		cluster:    "/api/clustermgmt/" + opts.ClusterVersion + "/config",
		prism:      "/api/prism/" + opts.PrismVersion + "/config",
		http:       &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }},
	}, nil
}

type httpError struct {
	status       int
	method, path string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("Prism Central v4: %s %s: HTTP %d", e.method, e.path, e.status)
}

// Response bodies are intentionally excluded from errors: Prism may echo user-data.
func (c *v4Client) do(ctx context.Context, method, path string, body, out any, headers http.Header) (http.Header, error) {
	return c.doWithResponseLimit(ctx, method, path, body, out, headers, 16<<20)
}

func (c *v4Client) doWithResponseLimit(ctx context.Context, method, path string, body, out any, headers http.Header, limit int64) (http.Header, error) {
	var payload []byte
	var err error
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode v4 request: %w", err)
		}
	}
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		for k, values := range headers {
			req.Header[k] = values
		}
		// Always send exactly one authentication mechanism, including on retries.
		req.Header.Del("Authorization")
		req.Header.Del("X-Ntnx-Api-Key")
		if c.apiKey != "" {
			req.Header.Set("X-Ntnx-Api-Key", c.apiKey)
		} else {
			req.SetBasicAuth(c.user, c.pass)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("Prism Central %s request failed: %w", method, err)
		}
		retryable := resp.StatusCode == 429 || resp.StatusCode == 502 || resp.StatusCode == 503 || resp.StatusCode == 504
		if retryable && attempt < 2 && (method == http.MethodGet || req.Header.Get("Ntnx-Request-Id") != "") {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			delay := time.Duration(1<<attempt) * time.Second
			if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && seconds > 0 && seconds <= 30 {
				delay = time.Duration(seconds) * time.Second
			}
			if err := wait(ctx, delay); err != nil {
				return nil, err
			}
			continue
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, &httpError{resp.StatusCode, method, strings.Split(path, "?")[0]}
		}
		if out != nil && resp.StatusCode != http.StatusNoContent {
			raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
			if err != nil || int64(len(raw)) > limit {
				return nil, fmt.Errorf("Prism Central v4 response incomplete or exceeds size limit")
			}
			if err := json.Unmarshal(raw, out); err != nil {
				return nil, fmt.Errorf("decode Prism Central v4 response: %w", err)
			}
		}
		return resp.Header, nil
	}
	panic("unreachable")
}

func wait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type envelope[T any] struct {
	Data     T `json:"data"`
	Metadata struct {
		Total *int `json:"totalAvailableResults"`
	} `json:"metadata"`
}

// list walks all result pages, including exactly-full final pages.
func list[T any](ctx context.Context, c *v4Client, path, filter string) ([]T, error) {
	var all []T
	for page := 0; ; page++ {
		q := url.Values{"$page": {strconv.Itoa(page)}, "$limit": {"100"}}
		if filter != "" {
			q.Set("$filter", filter)
		}
		var response envelope[[]T]
		if _, err := c.do(ctx, http.MethodGet, path+"?"+q.Encode(), nil, &response, nil); err != nil {
			return nil, err
		}
		all = append(all, response.Data...)
		if len(response.Data) == 0 || (response.Metadata.Total != nil && len(all) >= *response.Metadata.Total) || len(response.Data) < 100 {
			return all, nil
		}
	}
}

func nameFilter(name string) string { return "name eq '" + strings.ReplaceAll(name, "'", "''") + "'" }

type entity struct {
	UUID              string   `json:"extId"`
	Name              string   `json:"name"`
	ContainerUUID     string   `json:"containerExtId"`
	ClusterUUID       string   `json:"clusterExtId"`
	ClusterReference  string   `json:"clusterReference"`
	VPCUUID           string   `json:"vpcReference"`
	SubnetType        string   `json:"subnetType"`
	IsExternal        bool     `json:"isExternal"`
	ClusterReferences []string `json:"clusterReferenceList"`
	Config            struct {
		Functions   []string `json:"clusterFunction"`
		Hypervisors []string `json:"hypervisorTypes"`
	} `json:"config"`
}

func (c *v4Client) resolve(ctx context.Context, path, kind string, selector Selector, match func(entity) bool) (entity, error) {
	var candidates []entity
	if selector.UUID != "" {
		var response envelope[entity]
		if _, err := c.do(ctx, http.MethodGet, path+"/"+url.PathEscape(selector.UUID), nil, &response, nil); err != nil {
			return entity{}, err
		}
		candidates = []entity{response.Data}
	} else {
		if selector.Name == "" {
			return entity{}, fmt.Errorf("%s requires a name or UUID", kind)
		}
		var err error
		candidates, err = list[entity](ctx, c, path, nameFilter(selector.Name))
		if err != nil {
			return entity{}, err
		}
	}
	var found []entity
	for _, candidate := range candidates {
		if candidate.UUID == "" {
			candidate.UUID = candidate.ContainerUUID
		}
		if candidate.UUID == "" || (selector.UUID != "" && candidate.UUID != selector.UUID) || (selector.Name != "" && candidate.Name != selector.Name) {
			continue
		}
		if match == nil || match(candidate) {
			found = append(found, candidate)
		}
	}
	if len(found) == 0 {
		return entity{}, fmt.Errorf("no matching %s for name=%q UUID=%q in the requested scope", kind, selector.Name, selector.UUID)
	}
	if len(found) > 1 {
		return entity{}, fmt.Errorf("ambiguous %s name %q (%d matches); use a UUID or narrower scope", kind, selector.Name, len(found))
	}
	return found[0], nil
}

func (c *v4Client) ResolveCluster(ctx context.Context, selector Selector) (string, error) {
	cluster, err := c.resolve(ctx, c.cluster+"/clusters", "AHV cluster", selector, func(e entity) bool {
		for _, function := range e.Config.Functions {
			if function == "PRISM_CENTRAL" {
				return false
			}
		}
		for _, hypervisor := range e.Config.Hypervisors {
			if hypervisor == "AHV" {
				return true
			}
		}
		return false
	})
	return cluster.UUID, err
}

func (c *v4Client) ResolveContainer(ctx context.Context, selector Selector, clusterUUID string) (string, error) {
	container, err := c.resolve(ctx, c.cluster+"/storage-containers", "storage container", selector, func(e entity) bool { return e.ClusterUUID == clusterUUID })
	if container.ContainerUUID != "" {
		return container.ContainerUUID, err
	}
	return container.UUID, err
}

func (c *v4Client) ResolveSubnet(ctx context.Context, selector SubnetSelector) (string, error) {
	var vpcUUID string
	if selector.VPC.UUID != "" || selector.VPC.Name != "" {
		vpc, err := c.resolve(ctx, c.networking+"/vpcs", "VPC", selector.VPC, nil)
		if err != nil {
			return "", err
		}
		vpcUUID = vpc.UUID
	}
	subnet, err := c.resolve(ctx, c.networking+"/subnets", "subnet", selector.Selector, func(e entity) bool {
		if e.IsExternal {
			return false
		} // External VPC uplinks are not guest networks.
		if e.SubnetType == "OVERLAY" {
			return vpcUUID == "" || e.VPCUUID == vpcUUID
		}
		if e.SubnetType != "VLAN" || vpcUUID != "" {
			return false
		}
		if e.ClusterReference == selector.ClusterUUID {
			return true
		}
		for _, id := range e.ClusterReferences {
			if id == selector.ClusterUUID {
				return true
			}
		}
		return false
	})
	return subnet.UUID, err
}

type image struct {
	UUID         string   `json:"extId"`
	Name         string   `json:"name"`
	Type         string   `json:"type"`
	SizeBytes    int64    `json:"sizeBytes"`
	Description  string   `json:"description"`
	ClusterUUIDs []string `json:"clusterLocationExtIds"`
	Source       struct {
		ObjectType string `json:"$objectType"`
		URL        string `json:"url"`
	} `json:"source"`
	Checksum struct {
		ObjectType string `json:"$objectType"`
		HexDigest  string `json:"hexDigest"`
	} `json:"checksum"`
}

func imageResult(raw image) *Image {
	status := ImageStatusPending
	if raw.UUID != "" && raw.SizeBytes > 0 {
		status = ImageStatusAvailable
	}
	result := &Image{UUID: raw.UUID, Name: raw.Name, Type: raw.Type, SizeBytes: raw.SizeBytes, Status: status,
		Description: raw.Description, SourceURL: raw.Source.URL, SourceType: raw.Source.ObjectType, ClusterUUIDs: raw.ClusterUUIDs}
	if raw.Checksum.ObjectType == "vmm.v4.content.ImageSha256Checksum" {
		result.SHA256 = strings.ToLower(raw.Checksum.HexDigest)
	}
	return result
}

func (c *v4Client) GetImage(ctx context.Context, id string) (*Image, error) {
	var response envelope[image]
	_, err := c.do(ctx, http.MethodGet, c.vmm+"/content/images/"+url.PathEscape(id), nil, &response, nil)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(response.Data.UUID, id) {
		return nil, fmt.Errorf("Prism Central image response identity mismatch")
	}
	return imageResult(response.Data), nil
}

func (c *v4Client) ResolveImage(ctx context.Context, selector Selector) (*Image, error) {
	if selector.UUID != "" {
		img, err := c.GetImage(ctx, selector.UUID)
		if err == nil && selector.Name != "" && selector.Name != img.Name {
			return nil, fmt.Errorf("image UUID does not match image_name")
		}
		return img, err
	}
	found, err := c.FindImage(ctx, selector.Name)
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, fmt.Errorf("image %q not found; upload it to Prism Central first", selector.Name)
	}
	return found, nil
}

// FindImage uses LIST only to discover one exact identity. Verification metadata
// always comes from a fresh GET, never from a potentially incomplete list view.
func (c *v4Client) FindImage(ctx context.Context, name string) (*Image, error) {
	if name == "" {
		return nil, fmt.Errorf("image name is required")
	}
	images, err := list[image](ctx, c, c.vmm+"/content/images", nameFilter(name))
	if err != nil {
		return nil, err
	}
	var found *Image
	for _, img := range images {
		if img.Name != name {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("ambiguous image name %q; use image_uuid", name)
		}
		found = imageResult(img)
	}
	if found == nil {
		return nil, nil
	}
	if found.UUID == "" {
		return nil, fmt.Errorf("Prism Central image lookup returned no identity")
	}
	detail, err := c.GetImage(ctx, found.UUID)
	if err != nil {
		return nil, err // Including 404: a vanished candidate is not permission to POST.
	}
	if detail.Name != name {
		return nil, fmt.Errorf("Prism Central image name changed during lookup; refusing reselection")
	}
	return detail, nil
}

type rawVMDisk struct {
	UUID    string `json:"extId"`
	Address struct {
		Bus   string `json:"busType"`
		Index *int   `json:"index"`
	} `json:"diskAddress"`
	Backing struct {
		ObjectType string `json:"$objectType"`
		Size       int64  `json:"diskSizeBytes"`
		UUID       string `json:"diskExtId"`
		Migrating  *bool  `json:"isMigrationInProgress"`
		Container  struct {
			UUID string `json:"extId"`
		} `json:"storageContainer"`
	} `json:"backingInfo"`
}

func diskResult(disk rawVMDisk) VMDisk {
	index := -1
	if disk.Address.Index != nil {
		index = *disk.Address.Index
	}
	return VMDisk{
		UUID: disk.Backing.UUID, DeviceUUID: disk.UUID,
		Bus: disk.Address.Bus, Index: index, BackingType: disk.Backing.ObjectType,
		SizeBytes: disk.Backing.Size, ContainerUUID: disk.Backing.Container.UUID,
		MigrationInFlight: disk.Backing.Migrating != nil && *disk.Backing.Migrating,
		MigrationKnown:    disk.Backing.Migrating != nil,
	}
}

type rawVM struct {
	UUID                  string      `json:"extId"`
	BIOSUUID              string      `json:"biosUuid"`
	Name                  string      `json:"name"`
	Description           string      `json:"description"`
	PowerState            string      `json:"powerState"`
	CrossClusterMigrating *bool       `json:"isCrossClusterMigrationInProgress"`
	Disks                 []rawVMDisk `json:"disks"`
	GPUs                  []vmGPU     `json:"gpus"`
	CDROMs                []struct {
		rawVMDisk
		ISOType string `json:"isoType"`
	} `json:"cdRoms"`
	Categories []struct {
		UUID string `json:"extId"`
	} `json:"categories"`
	Cluster struct {
		UUID string `json:"extId"`
	} `json:"cluster"`
}

func vmResult(vm rawVM, etag string) *VM {
	result := &VM{UUID: vm.UUID, BIOSUUID: vm.BIOSUUID, Name: vm.Name, Description: vm.Description, PowerState: vm.PowerState, ClusterUUID: vm.Cluster.UUID, ETag: etag}
	result.CrossClusterMigrationKnown = vm.CrossClusterMigrating != nil
	result.CrossClusterMigrationInFlight = vm.CrossClusterMigrating != nil && *vm.CrossClusterMigrating
	for _, category := range vm.Categories {
		result.CategoryUUIDs = append(result.CategoryUUIDs, category.UUID)
	}
	for _, disk := range vm.Disks {
		result.Disks = append(result.Disks, diskResult(disk))
	}
	for _, gpu := range vm.GPUs {
		result.GPUs = append(result.GPUs, GPUConfig{Mode: gpu.Mode, Vendor: gpu.Vendor, DeviceID: gpu.DeviceID})
	}
	for _, cd := range vm.CDROMs {
		result.CDROMs = append(result.CDROMs, VMCDROM{VMDisk: diskResult(cd.rawVMDisk), ISOType: cd.ISOType})
	}
	return result
}

func (c *v4Client) GetVM(ctx context.Context, id string) (*VM, error) {
	var response envelope[rawVM]
	headers, err := c.do(ctx, http.MethodGet, c.vmm+"/ahv/config/vms/"+url.PathEscape(id), nil, &response, nil)
	if err != nil {
		return nil, err
	}
	if response.Data.UUID == "" {
		return nil, fmt.Errorf("VM response has no extId")
	}
	return vmResult(response.Data, headers.Get("ETag")), nil
}

func (c *v4Client) FindVM(ctx context.Context, name, biosUUID, clusterUUID string) (*VM, error) {
	vms, err := list[rawVM](ctx, c, c.vmm+"/ahv/config/vms", nameFilter(name))
	if err != nil {
		return nil, err
	}
	var found *VM
	for _, vm := range vms {
		if vm.Name != name || !strings.EqualFold(vm.BIOSUUID, biosUUID) || vm.Cluster.UUID != clusterUUID {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("multiple VMs have provider BIOS UUID %s; refusing to choose", biosUUID)
		}
		found = vmResult(vm, "")
	}
	return found, nil
}

type taskRef struct {
	ObjectType string `json:"$objectType"`
	UUID       string `json:"extId"` // Historical Go name; this is an opaque task ID.
}

func (c *v4Client) mutation(ctx context.Context, method, path string, body any, requestID, etag string) (string, error) {
	headers := http.Header{"Ntnx-Request-Id": {requestID}}
	if etag != "" {
		headers.Set("If-Match", etag)
	}
	var response envelope[taskRef]
	if _, err := c.do(ctx, method, path, body, &response, headers); err != nil {
		return "", err
	}
	if response.Data.UUID == "" || response.Data.ObjectType != "prism.v4.config.TaskReference" {
		return "", fmt.Errorf("v4 mutation returned no task reference")
	}
	if err := ValidateTaskID(response.Data.UUID); err != nil {
		return "", err
	}
	return response.Data.UUID, nil
}

func (c *v4Client) CreateVM(ctx context.Context, spec VMCreateSpec) (string, error) {
	body, err := buildVM(spec)
	if err != nil {
		return "", err
	}
	return c.mutation(ctx, http.MethodPost, c.vmm+"/ahv/config/vms", body, spec.RequestUUID, "")
}

func (c *v4Client) changeVM(ctx context.Context, id, action string) (string, error) {
	vm, err := c.GetVM(ctx, id)
	if err != nil {
		return "", err
	}
	if vm.ETag == "" {
		return "", fmt.Errorf("VM %s has no ETag; refusing unsafe mutation", id)
	}
	method, path := http.MethodPost, c.vmm+"/ahv/config/vms/"+url.PathEscape(id)
	if action == "delete" {
		method = http.MethodDelete
	} else {
		path += "/$actions/" + action
	}
	requestID := uuid.NewSHA1(uuid.NameSpaceURL, []byte(c.base+"/"+id+"/"+action+"/"+vm.ETag)).String()
	return c.mutation(ctx, method, path, nil, requestID, vm.ETag)
}

func (c *v4Client) PowerOnVM(ctx context.Context, id string) (string, error) {
	return c.changeVM(ctx, id, "power-on")
}
func (c *v4Client) PowerOffVM(ctx context.Context, id string) (string, error) {
	return c.changeVM(ctx, id, "power-off")
}
func (c *v4Client) DeleteVM(ctx context.Context, id string) (string, error) {
	return c.changeVM(ctx, id, "delete")
}

func (c *v4Client) WaitTask(ctx context.Context, id string) (TaskResult, error) {
	if err := ValidateTaskID(id); err != nil {
		return TaskResult{}, err
	}
	var response envelope[struct {
		ID       string `json:"extId"`
		Status   string `json:"status"`
		Entities []struct {
			UUID string `json:"extId"`
			Rel  string `json:"rel"`
		} `json:"entitiesAffected"`
		Errors json.RawMessage `json:"errorMessages"`
	}]
	if _, err := c.do(ctx, http.MethodGet, c.prism+"/tasks/"+url.PathEscape(id), nil, &response, nil); err != nil {
		return TaskResult{}, err
	}
	if response.Data.ID != "" && response.Data.ID != id {
		return TaskResult{}, fmt.Errorf("Prism Central task response identity mismatch; operator review required")
	}
	result := TaskResult{State: TaskPending}
	switch response.Data.Status {
	case "SUCCEEDED":
		result.State = TaskSucceeded
	case "FAILED", "CANCELED":
		result.State = TaskFailed
	case "QUEUED", "RUNNING", "CANCELING", "SUSPENDED":
		return result, nil // Never use a nonterminal reference to release teardown.
	default:
		return TaskResult{}, fmt.Errorf("unknown Prism Central task status %q", response.Data.Status)
	}
	// Failed creation may leave a VM behind or name one that is already absent.
	// Preserve the reference for identity-aware teardown, not provisioning retry.
	for _, entity := range response.Data.Entities {
		if entity.Rel != "vmm:ahv:vm" && entity.Rel != "vmm:ahv:config:vm" {
			continue
		}
		vmID, err := uuid.Parse(entity.UUID)
		if err != nil || vmID == uuid.Nil {
			// Return no terminal outcome on invalid/ambiguous identity so callers
			// cannot ignore the error merely because the task was FAILED.
			return TaskResult{}, fmt.Errorf("task %s has an invalid VM reference; operator review required", id)
		}
		if result.VMUUID != "" && result.VMUUID != vmID.String() {
			return TaskResult{}, fmt.Errorf("task %s affected multiple VMs; operator review required", id)
		}
		result.VMUUID = vmID.String()
	}
	if result.State == TaskFailed {
		result.MigrationLimitRejected = response.Data.Status == "FAILED" && migrationLimitOnly(response.Data.Errors)
		return result, fmt.Errorf("Prism Central task %s %s; inspect task details in Prism Central", id, response.Data.Status)
	}
	return result, nil
}

// Codes are structured evidence, never guessed from localized/untrusted message
// text. Malformed/missing/mixed codes disable retry without losing terminal task
// identity needed for ordinary teardown. Raw messages/arguments are not retained.
func migrationLimitOnly(raw json.RawMessage) bool {
	var messages []struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(raw, &messages) != nil || len(messages) == 0 || len(messages) > 16 {
		return false
	}
	for _, message := range messages {
		if message.Code != "VMM-30716" {
			return false
		}
	}
	return true
}

func (c *v4Client) IsNotFound(err error) bool {
	var response *httpError
	return errors.As(err, &response) && response.status == http.StatusNotFound
}
