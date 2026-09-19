// Package client is the onyx-sdk Go client. It talks to the onyx-api gateway
// over its REST surface (docs/design/06), so scripts and apps interact with
// Onyx exactly like the UI does.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultEndpoint matches the dev default of onyx-api.
const DefaultEndpoint = "http://127.0.0.1:8080"

// Client is a REST client for the onyx-api gateway.
type Client struct {
	endpoint string
	http     *http.Client
	token    string // optional Bearer token (docs/design/06#1-conventions)
}

// New returns a client talking to endpoint (e.g. "http://192.168.1.5:8080").
func New(endpoint string) *Client {
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	return &Client{
		endpoint: strings.TrimSuffix(endpoint, "/"),
		http:     &http.Client{Timeout: 30 * time.Second},
	}
}

// WithToken returns a copy of the client that authenticates with the given
// bearer token on every request.
func (c *Client) WithToken(token string) *Client {
	cp := *c
	cp.token = token
	return &cp
}

// APIError is the error envelope defined in docs/design/06#2-error-model.
type APIError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
	Retryable bool   `json:"retryable"`
}

func (e *APIError) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

// Error is returned for transport- or protocol-level failures.
type Error struct{ Err error }

func (e *Error) Error() string { return fmt.Sprintf("onyx client: %v", e.Err) }
func (e *Error) Unwrap() error { return e.Err }

// Version is the response of GET /api/v1/system/version.
type Version struct {
	Version    string `json:"version"`
	APIVersion string `json:"api_version"`
	Codename   string `json:"codename"`
	Commit     string `json:"commit,omitempty"`
}

// ServiceStatus mirrors onyx.v1.ServiceStatus (protojson camelCase).
type ServiceStatus struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Status  string `json:"status"` // SERVING | NOT_SERVING | UNKNOWN
}

// SystemStatus mirrors onyx.v1.SystemStatusResponse.
type SystemStatus struct {
	CoreVersion string          `json:"coreVersion"`
	Services    []ServiceStatus `json:"services"`
}

// Pool mirrors onyx.v1.Pool.
type Pool struct {
	Name       string `json:"name"`
	UUID       string `json:"uuid"`
	FSType     string `json:"fsType"`
	TotalBytes uint64 `json:"totalBytes"`
	UsedBytes  uint64 `json:"usedBytes"`
	State      string `json:"state"`
}

// UnmarshalJSON accepts both a JSON number and the proto3 JSON string form for
// 64-bit fields (protojson serializes uint64 as strings to keep clients safe).
func (p *Pool) UnmarshalJSON(b []byte) error {
	var raw struct {
		Name       string          `json:"name"`
		UUID       string          `json:"uuid"`
		FSType     string          `json:"fsType"`
		TotalBytes json.RawMessage `json:"totalBytes"`
		UsedBytes  json.RawMessage `json:"usedBytes"`
		State      string          `json:"state"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*p = Pool{Name: raw.Name, UUID: raw.UUID, FSType: raw.FSType, State: raw.State}
	var err error
	if p.TotalBytes, err = parseUint64(raw.TotalBytes); err != nil {
		return fmt.Errorf("totalBytes: %w", err)
	}
	if p.UsedBytes, err = parseUint64(raw.UsedBytes); err != nil {
		return fmt.Errorf("usedBytes: %w", err)
	}
	return nil
}

func parseUint64(raw json.RawMessage) (uint64, error) {
	s := string(raw)
	// unwrap a JSON string (protojson emits 64-bit fields as strings)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	return strconv.ParseUint(s, 10, 64)
}

// parseInt64 is parseUint64 for signed 64-bit fields, accepting both the JSON
// number form and the protojson string form. An absent field reads as 0, so a
// response that omits a counter is not an error.
func parseInt64(raw json.RawMessage) (int64, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return 0, nil
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	if s == "" {
		return 0, nil
	}
	return strconv.ParseInt(s, 10, 64)
}

// Pools is the response of GET /api/v1/pools.
type Pools struct {
	Pools []Pool `json:"pools"`
}

// Device mirrors onyx.v1.Device (protojson camelCase). State is
// attached | mounted | detached; auto is removable | all | manual;
// healthStatus is ok | degraded | unknown.
type Device struct {
	Name         string `json:"name"`
	KName        string `json:"kName"`
	Path         string `json:"path"`
	Type         string `json:"type"`
	FSType       string `json:"fsType"`
	Label        string `json:"label"`
	UUID         string `json:"uuid"`
	SizeBytes    uint64 `json:"sizeBytes"`
	Mountpoint   string `json:"mountpoint"`
	Removable    bool   `json:"removable"`
	State        string `json:"state"`
	Auto         string `json:"auto"`
	HealthStatus string `json:"healthStatus"`
	TemperatureC uint32 `json:"temperatureC"`
}

// UnmarshalJSON accepts both a JSON number and the proto3 JSON string form
// for 64-bit fields (protojson serializes uint64 as strings).
func (d *Device) UnmarshalJSON(b []byte) error {
	var raw struct {
		Name         string          `json:"name"`
		KName        string          `json:"kName"`
		Path         string          `json:"path"`
		Type         string          `json:"type"`
		FSType       string          `json:"fsType"`
		Label        string          `json:"label"`
		UUID         string          `json:"uuid"`
		SizeBytes    json.RawMessage `json:"sizeBytes"`
		Mountpoint   string          `json:"mountpoint"`
		Removable    bool            `json:"removable"`
		State        string          `json:"state"`
		Auto         string          `json:"auto"`
		HealthStatus string          `json:"healthStatus"`
		TemperatureC uint32          `json:"temperatureC"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*d = Device{
		Name: raw.Name, KName: raw.KName, Path: raw.Path, Type: raw.Type,
		FSType: raw.FSType, Label: raw.Label, UUID: raw.UUID,
		Mountpoint: raw.Mountpoint, Removable: raw.Removable,
		State: raw.State, Auto: raw.Auto,
		HealthStatus: raw.HealthStatus, TemperatureC: raw.TemperatureC,
	}
	size, err := parseUint64(raw.SizeBytes)
	if err != nil {
		return fmt.Errorf("sizeBytes: %w", err)
	}
	d.SizeBytes = size
	return nil
}

// Devices is the response of GET /api/v1/devices.
type Devices struct {
	Devices []Device `json:"devices"`
}

// DeviceEvent mirrors onyx.v1.DeviceEvent. Event is
// attach | detach | health | error; Detail carries the mountpoint, the
// reason ("unplugged"/"detached") or the health verdict ("ok temp=38C").
type DeviceEvent struct {
	ID     uint64 `json:"id"`
	TS     string `json:"ts"`
	KName  string `json:"kName"`
	Name   string `json:"name"`
	Event  string `json:"event"`
	Detail string `json:"detail"`
}

// UnmarshalJSON handles the proto3 JSON string form of the uint64 id.
func (e *DeviceEvent) UnmarshalJSON(b []byte) error {
	var raw struct {
		ID     json.RawMessage `json:"id"`
		TS     string          `json:"ts"`
		KName  string          `json:"kName"`
		Name   string          `json:"name"`
		Event  string          `json:"event"`
		Detail string          `json:"detail"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	id, err := parseUint64(raw.ID)
	if err != nil {
		return fmt.Errorf("id: %w", err)
	}
	*e = DeviceEvent{ID: id, TS: raw.TS, KName: raw.KName, Name: raw.Name, Event: raw.Event, Detail: raw.Detail}
	return nil
}

// Events is the response of GET /api/v1/events.
type Events struct {
	Events []DeviceEvent `json:"events"`
}

// --- API methods ---

// SystemVersion returns core + API versions (GET /api/v1/system/version).
func (c *Client) SystemVersion(ctx context.Context) (*Version, error) {
	var v Version
	if err := c.getJSON(ctx, "/api/v1/system/version", &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// SystemStatus returns aggregate health (GET /api/v1/system/status).
func (c *Client) SystemStatus(ctx context.Context) (*SystemStatus, error) {
	var s SystemStatus
	if err := c.getJSON(ctx, "/api/v1/system/status", &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// ListPools returns the storage pools (GET /api/v1/pools).
func (c *Client) ListPools(ctx context.Context) (*Pools, error) {
	var p Pools
	if err := c.getJSON(ctx, "/api/v1/pools", &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// GetPool returns one storage pool by name (GET /api/v1/pools/{name}).
// Returns an *APIError with Code "not_found" when the pool does not exist.
func (c *Client) GetPool(ctx context.Context, name string) (*Pool, error) {
	var p Pool
	if err := c.getJSON(ctx, "/api/v1/pools/"+url.PathEscape(name), &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// CreatePoolRequest is the body of POST /api/v1/pools. Device is a device
// name or kernel name; Name becomes the filesystem label and pool name.
// Force and AutoMount nil mean the server defaults (both true): creating a
// pool force-unmounts busy mounts, overwrites existing signatures and remounts.
type CreatePoolRequest struct {
	Device    string `json:"device"`
	Name      string `json:"name"`
	FSType    string `json:"fs_type,omitempty"`
	Force     *bool  `json:"force,omitempty"`
	AutoMount *bool  `json:"auto_mount,omitempty"`
	MountName string `json:"mount_name,omitempty"`
}

// CreatePool formats a verified removable whole disk as btrfs or ext4 and
// mounts the new filesystem under /mnt/onyx (POST /api/v1/pools). The data
// plane unmounts the disk first (forcing a busy mount) so an existing pool on
// the same disk is erased and re-mounted in one step.
func (c *Client) CreatePool(ctx context.Context, req *CreatePoolRequest) (*Pool, error) {
	var p Pool
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/pools", req, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// DeletePool forgets a pool record by name (DELETE /api/v1/pools/{name}).
// The pool's mount is released and its registry row dropped; the filesystem on
// the device is never erased, so the pool can be created again or imported
// later. A stale record — a pool whose disk was re-formatted, relabelled or
// pulled — is what this clears.
func (c *Client) DeletePool(ctx context.Context, name string) error {
	return c.doJSON(ctx, http.MethodDelete, "/api/v1/pools/"+url.PathEscape(name), nil, nil)
}

// RemoteProvider describes one cloud/remote backend the API can configure
// (GET /api/v1/storage/providers). Field names are rclone's own option names.
type RemoteProvider struct {
	Type     string   `json:"type"`
	Label    string   `json:"label"`
	Fields   []string `json:"fields"`
	Required []string `json:"required"`
	OAuth    bool     `json:"oauth"`
	Hint     string   `json:"hint,omitempty"`
}

// Remotes is the response of GET /api/v1/storage/remotes. Details maps a
// remote name to its backend type.
type Remotes struct {
	Configured bool              `json:"configured"`
	Remotes    []string          `json:"remotes"`
	Details    map[string]string `json:"details,omitempty"`
	Providers  []RemoteProvider  `json:"providers,omitempty"`
}

// CreateRemoteRequest is the body of POST /api/v1/storage/remotes.
type CreateRemoteRequest struct {
	Name   string            `json:"name"`
	Type   string            `json:"type"`
	Params map[string]string `json:"params"`
}

// Remote is the result of creating a remote. NextStep carries the one-time
// browser approval an OAuth backend still needs.
type Remote struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	OAuth    bool   `json:"oauth"`
	NextStep string `json:"next_step,omitempty"`
}

// RemoteCheck is the reachability probe of POST /api/v1/storage/remotes/{name}/check.
type RemoteCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// CloneToRemoteRequest is the body of POST /api/v1/storage/clone. Source is
// relative to the storage root; Dest is the folder inside the remote.
type CloneToRemoteRequest struct {
	Source string `json:"source"`
	Remote string `json:"remote"`
	Dest   string `json:"dest,omitempty"`
}

// CloneResult reports a finished rclone copy to a remote.
type CloneResult struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	Detail   string `json:"detail"`
	Finished bool   `json:"finished"`
}

// ListRemotes returns the configured remotes and the provider catalog
// (GET /api/v1/storage/remotes).
func (c *Client) ListRemotes(ctx context.Context) (*Remotes, error) {
	var remotes Remotes
	if err := c.getJSON(ctx, "/api/v1/storage/remotes", &remotes); err != nil {
		return nil, err
	}
	if remotes.Remotes == nil {
		remotes.Remotes = []string{}
	}
	return &remotes, nil
}

// CreateRemote configures a cloud/remote backend (POST /api/v1/storage/remotes).
func (c *Client) CreateRemote(ctx context.Context, req *CreateRemoteRequest) (*Remote, error) {
	var remote Remote
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/storage/remotes", req, &remote); err != nil {
		return nil, err
	}
	return &remote, nil
}

// DeleteRemote removes a configured remote (DELETE /api/v1/storage/remotes/{name}).
func (c *Client) DeleteRemote(ctx context.Context, name string) error {
	return c.delete(ctx, "/api/v1/storage/remotes/"+url.PathEscape(name))
}

// CheckRemote probes a remote for reachability
// (POST /api/v1/storage/remotes/{name}/check).
func (c *Client) CheckRemote(ctx context.Context, name string) (*RemoteCheck, error) {
	var check RemoteCheck
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/storage/remotes/"+url.PathEscape(name)+"/check", nil, &check); err != nil {
		return nil, err
	}
	return &check, nil
}

// CloneToRemote copies a storage folder out to a configured remote
// (POST /api/v1/storage/clone).
func (c *Client) CloneToRemote(ctx context.Context, req *CloneToRemoteRequest) (*CloneResult, error) {
	var result CloneResult
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/storage/clone", req, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// --- Platform surfaces (v0.4 "Jade", docs/design/11 §6.3-§6.6) ---

// App mirrors onyx.v1.App (protojson camelCase). Status is one of
// not_installed | installed | updating | error: the app store and the installed
// list are the same catalog filtered by status.
type App struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Version     string            `json:"version"`
	Description string            `json:"description"`
	Status      string            `json:"status"`
	InstalledAt string            `json:"installedAt"`
	Config      map[string]string `json:"config"`
}

// Apps is the response of GET /api/v1/apps and /api/v1/app-store.
type Apps struct {
	Apps []App `json:"apps"`
}

// Container is one container of an installed app.
type Container struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Image   string `json:"image"`
	Status  string `json:"status"`
	AppID   string `json:"appId"`
	Service string `json:"service"`
}

// Containers is the response of GET /api/v1/containers.
type Containers struct {
	Containers []Container `json:"containers"`
}

// InstallAppRequest is the body of POST /api/v1/apps.
type InstallAppRequest struct {
	AppID   string            `json:"app_id"`
	Version string            `json:"version,omitempty"`
	Config  map[string]string `json:"config,omitempty"`
}

// ListApps returns the app catalog (GET /api/v1/apps). status filters it:
// "installed", "not_installed", "updating", or empty for the whole store.
func (c *Client) ListApps(ctx context.Context, status string) (*Apps, error) {
	path := "/api/v1/apps"
	if status != "" {
		path += "?status=" + url.QueryEscape(status)
	}
	var apps Apps
	if err := c.getJSON(ctx, path, &apps); err != nil {
		return nil, err
	}
	if apps.Apps == nil {
		apps.Apps = []App{}
	}
	return &apps, nil
}

// InstallApp installs an app from the catalog (POST /api/v1/apps).
func (c *Client) InstallApp(ctx context.Context, req *InstallAppRequest) (*App, error) {
	var app App
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/apps", req, &app); err != nil {
		return nil, err
	}
	return &app, nil
}

// UninstallApp removes an app (DELETE /api/v1/apps/{id}). purgeData deletes the
// app's volumes; force stops running containers instead of refusing.
func (c *Client) UninstallApp(ctx context.Context, id string, purgeData, force bool) error {
	path := "/api/v1/apps/" + url.PathEscape(id) + fmt.Sprintf("?purge_data=%t&force=%t", purgeData, force)
	return c.delete(ctx, path)
}

// ListContainers returns containers, optionally filtered by app
// (GET /api/v1/containers).
func (c *Client) ListContainers(ctx context.Context, appID string) (*Containers, error) {
	path := "/api/v1/containers"
	if appID != "" {
		path += "?app_id=" + url.QueryEscape(appID)
	}
	var containers Containers
	if err := c.getJSON(ctx, path, &containers); err != nil {
		return nil, err
	}
	if containers.Containers == nil {
		containers.Containers = []Container{}
	}
	return &containers, nil
}

// ContainerAction starts, stops or restarts one container
// (POST /api/v1/containers/{id}/{action}).
func (c *Client) ContainerAction(ctx context.Context, id, action string) (*Container, error) {
	var container Container
	path := "/api/v1/containers/" + url.PathEscape(id) + "/" + action
	if err := c.doJSON(ctx, http.MethodPost, path, nil, &container); err != nil {
		return nil, err
	}
	return &container, nil
}

// VM mirrors onyx.v1.VM. Status is stopped | running | paused | error.
type VM struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	VCPUs     int32  `json:"vcpus"`
	MemoryMB  int64  `json:"memoryMb"`
	Disk      string `json:"disk"`
	OS        string `json:"os"`
	CreatedAt string `json:"createdAt"`
}

// UnmarshalJSON accepts the proto3 JSON string form of the int64 memory size.
func (v *VM) UnmarshalJSON(b []byte) error {
	var raw struct {
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Status    string          `json:"status"`
		VCPUs     int32           `json:"vcpus"`
		MemoryMB  json.RawMessage `json:"memoryMb"`
		Disk      string          `json:"disk"`
		OS        string          `json:"os"`
		CreatedAt string          `json:"createdAt"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	memory, err := parseInt64(raw.MemoryMB)
	if err != nil {
		return fmt.Errorf("memoryMb: %w", err)
	}
	*v = VM{
		ID: raw.ID, Name: raw.Name, Status: raw.Status, VCPUs: raw.VCPUs,
		MemoryMB: memory, Disk: raw.Disk, OS: raw.OS, CreatedAt: raw.CreatedAt,
	}
	return nil
}

// VMs is the response of GET /api/v1/vms.
type VMs struct {
	VMs []VM `json:"vms"`
}

// CreateVMRequest is the body of POST /api/v1/vms. DiskMB of 0 takes the
// service default; ISO attaches install media.
type CreateVMRequest struct {
	Name     string `json:"name"`
	VCPUs    int32  `json:"vcpus"`
	MemoryMB int64  `json:"memory_mb"`
	DiskMB   int64  `json:"disk_mb,omitempty"`
	OS       string `json:"os,omitempty"`
	ISO      string `json:"iso,omitempty"`
}

// ListVMs returns the VM inventory (GET /api/v1/vms), reading each machine's
// live state from the hypervisor.
func (c *Client) ListVMs(ctx context.Context) (*VMs, error) {
	var vms VMs
	if err := c.getJSON(ctx, "/api/v1/vms", &vms); err != nil {
		return nil, err
	}
	if vms.VMs == nil {
		vms.VMs = []VM{}
	}
	return &vms, nil
}

// CreateVM defines a machine without booting it (POST /api/v1/vms).
func (c *Client) CreateVM(ctx context.Context, req *CreateVMRequest) (*VM, error) {
	var vm VM
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/vms", req, &vm); err != nil {
		return nil, err
	}
	return &vm, nil
}

// StartVM boots a machine (POST /api/v1/vms/{id}/start).
func (c *Client) StartVM(ctx context.Context, id string) (*VM, error) {
	var vm VM
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/vms/"+url.PathEscape(id)+"/start", nil, &vm); err != nil {
		return nil, err
	}
	return &vm, nil
}

// StopVM shuts a machine down (POST /api/v1/vms/{id}/stop); graceful asks the
// guest to shut down, otherwise the power is cut.
func (c *Client) StopVM(ctx context.Context, id string, graceful bool) (*VM, error) {
	var vm VM
	body := map[string]bool{"graceful": graceful}
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/vms/"+url.PathEscape(id)+"/stop", body, &vm); err != nil {
		return nil, err
	}
	return &vm, nil
}

// DeleteVM removes a machine (DELETE /api/v1/vms/{id}). deleteDisk also deletes
// the disk image, which is irreversible.
func (c *Client) DeleteVM(ctx context.Context, id string, deleteDisk bool) error {
	return c.delete(ctx, "/api/v1/vms/"+url.PathEscape(id)+fmt.Sprintf("?delete_disk=%t", deleteDisk))
}

// Bucket mirrors onyx.v1.Bucket. Tier is local | cloud | tiered; CloudTarget is
// an rclone remote (`name:` or `name:path`) for the cloud tiers. LocalObjects is
// a live count; CloudObjects is what the last verified sync recorded, empty
// until the bucket has been synced.
type Bucket struct {
	Name           string `json:"name"`
	Tier           string `json:"tier"`
	CloudTarget    string `json:"cloudTarget"`
	CreatedAt      string `json:"createdAt"`
	LocalObjects   int64  `json:"localObjects"`
	CloudObjects   int64  `json:"cloudObjects"`
	LastSyncAt     string `json:"lastSyncAt"`
	EvictAfterDays int32  `json:"evictAfterDays"`
	SyncError      string `json:"syncError"`
}

// UnmarshalJSON accepts the proto3 JSON string form of the int64 counters.
func (b *Bucket) UnmarshalJSON(raw []byte) error {
	var wire struct {
		Name           string          `json:"name"`
		Tier           string          `json:"tier"`
		CloudTarget    string          `json:"cloudTarget"`
		CreatedAt      string          `json:"createdAt"`
		LocalObjects   json.RawMessage `json:"localObjects"`
		CloudObjects   json.RawMessage `json:"cloudObjects"`
		LastSyncAt     string          `json:"lastSyncAt"`
		EvictAfterDays int32           `json:"evictAfterDays"`
		SyncError      string          `json:"syncError"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return err
	}
	local, err := parseInt64(wire.LocalObjects)
	if err != nil {
		return fmt.Errorf("localObjects: %w", err)
	}
	cloud, err := parseInt64(wire.CloudObjects)
	if err != nil {
		return fmt.Errorf("cloudObjects: %w", err)
	}
	*b = Bucket{
		Name: wire.Name, Tier: wire.Tier, CloudTarget: wire.CloudTarget,
		CreatedAt: wire.CreatedAt, LocalObjects: local, CloudObjects: cloud,
		LastSyncAt: wire.LastSyncAt, EvictAfterDays: wire.EvictAfterDays, SyncError: wire.SyncError,
	}
	return nil
}

// Buckets is the response of GET /api/v1/buckets.
type Buckets struct {
	Buckets []Bucket `json:"buckets"`
}

// CreateBucketRequest is the body of POST /api/v1/buckets.
type CreateBucketRequest struct {
	Name           string `json:"name"`
	Tier           string `json:"tier,omitempty"`
	CloudTarget    string `json:"cloud_target,omitempty"`
	EvictAfterDays int32  `json:"evict_after_days,omitempty"`
}

// SyncBucketResult reports a hybrid-cloud sync: how many objects the target
// gained, how many local copies were released, and any warning that explains
// why a step was skipped.
type SyncBucketResult struct {
	Bucket   Bucket   `json:"bucket"`
	Uploaded int64    `json:"uploaded"`
	Evicted  int64    `json:"evicted"`
	Detail   string   `json:"detail"`
	Warnings []string `json:"warnings"`
}

// UnmarshalJSON accepts the proto3 JSON string form of the int64 counters.
func (s *SyncBucketResult) UnmarshalJSON(raw []byte) error {
	var wire struct {
		Bucket   Bucket          `json:"bucket"`
		Uploaded json.RawMessage `json:"uploaded"`
		Evicted  json.RawMessage `json:"evicted"`
		Detail   string          `json:"detail"`
		Warnings []string        `json:"warnings"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return err
	}
	uploaded, err := parseInt64(wire.Uploaded)
	if err != nil {
		return fmt.Errorf("uploaded: %w", err)
	}
	evicted, err := parseInt64(wire.Evicted)
	if err != nil {
		return fmt.Errorf("evicted: %w", err)
	}
	*s = SyncBucketResult{Bucket: wire.Bucket, Uploaded: uploaded, Evicted: evicted, Detail: wire.Detail, Warnings: wire.Warnings}
	return nil
}

// ListBuckets returns the object-storage buckets (GET /api/v1/buckets).
func (c *Client) ListBuckets(ctx context.Context) (*Buckets, error) {
	var buckets Buckets
	if err := c.getJSON(ctx, "/api/v1/buckets", &buckets); err != nil {
		return nil, err
	}
	if buckets.Buckets == nil {
		buckets.Buckets = []Bucket{}
	}
	return &buckets, nil
}

// CreateBucket creates a bucket (POST /api/v1/buckets).
func (c *Client) CreateBucket(ctx context.Context, req *CreateBucketRequest) (*Bucket, error) {
	var bucket Bucket
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/buckets", req, &bucket); err != nil {
		return nil, err
	}
	return &bucket, nil
}

// DeleteBucket removes a bucket (DELETE /api/v1/buckets/{name}). force purges a
// non-empty bucket, local objects and the cloud target alike.
func (c *Client) DeleteBucket(ctx context.Context, name string, force bool) error {
	return c.delete(ctx, "/api/v1/buckets/"+url.PathEscape(name)+fmt.Sprintf("?force=%t", force))
}

// SyncBucket mirrors a cloud/tiered bucket into its target
// (POST /api/v1/buckets/{name}/sync). evict releases local copies the cloud has
// been verified to hold.
func (c *Client) SyncBucket(ctx context.Context, name string, evict bool) (*SyncBucketResult, error) {
	var result SyncBucketResult
	path := "/api/v1/buckets/" + url.PathEscape(name) + "/sync" + fmt.Sprintf("?evict=%t", evict)
	if err := c.doJSON(ctx, http.MethodPost, path, nil, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ShareProtocol identifies a protocol a share is exposed over.
type ShareProtocol string

const (
	ProtocolSMB    ShareProtocol = "SMB"
	ProtocolNFS    ShareProtocol = "NFS"
	ProtocolFTP    ShareProtocol = "FTP"
	ProtocolSFTP   ShareProtocol = "SFTP"
	ProtocolWebDAV ShareProtocol = "WEBDAV"
	ProtocolRsync  ShareProtocol = "RSYNC"
)

// Share mirrors onyx.v1.Share (protojson camelCase; protocols as enum names).
type Share struct {
	Name      string          `json:"name"`
	Path      string          `json:"path"`
	Comment   string          `json:"comment"`
	Readonly  bool            `json:"readonly"`
	Protocols []ShareProtocol `json:"protocols"`
}

// Shares is the response of GET /api/v1/shares.
type Shares struct {
	Shares []Share `json:"shares"`
}

// CreateShareRequest is the body of POST /api/v1/shares.
type CreateShareRequest struct {
	Name      string          `json:"name"`
	Path      string          `json:"path"`
	Comment   string          `json:"comment"`
	Readonly  bool            `json:"readonly"`
	Protocols []ShareProtocol `json:"protocols"`
}

// CreateShare creates a share (POST /api/v1/shares).
func (c *Client) CreateShare(ctx context.Context, req *CreateShareRequest) (*Share, error) {
	var out Share
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/shares", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListShares returns all shares (GET /api/v1/shares).
func (c *Client) ListShares(ctx context.Context) (*Shares, error) {
	var s Shares
	if err := c.getJSON(ctx, "/api/v1/shares", &s); err != nil {
		return nil, err
	}
	if s.Shares == nil {
		s.Shares = []Share{}
	}
	return &s, nil
}

// GetShare returns one share by name (GET /api/v1/shares/{name}).
func (c *Client) GetShare(ctx context.Context, name string) (*Share, error) {
	var s Share
	if err := c.getJSON(ctx, "/api/v1/shares/"+url.PathEscape(name), &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// DeleteShare deletes a share (DELETE /api/v1/shares/{name}).
func (c *Client) DeleteShare(ctx context.Context, name string) error {
	return c.delete(ctx, "/api/v1/shares/"+url.PathEscape(name))
}

// ListDevices returns every detected block device
// (GET /api/v1/devices).
func (c *Client) ListDevices(ctx context.Context) (*Devices, error) {
	var d Devices
	if err := c.getJSON(ctx, "/api/v1/devices", &d); err != nil {
		return nil, err
	}
	if d.Devices == nil {
		d.Devices = []Device{}
	}
	return &d, nil
}

// GetDevice returns one device by name (GET /api/v1/devices/{name}).
func (c *Client) GetDevice(ctx context.Context, name string) (*Device, error) {
	var d Device
	if err := c.getJSON(ctx, "/api/v1/devices/"+url.PathEscape(name), &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// MountDevice explicitly attaches (mounts) a device
// (POST /api/v1/devices/{name}/attach). Idempotent for already-mounted
// devices.
func (c *Client) MountDevice(ctx context.Context, name string) (*Device, error) {
	var d Device
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/devices/"+url.PathEscape(name)+"/attach", nil, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// UnmountDevice detaches a device onyx mounted
// (POST /api/v1/devices/{name}/detach).
func (c *Client) UnmountDevice(ctx context.Context, name string) (*Device, error) {
	var d Device
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/devices/"+url.PathEscape(name)+"/detach", nil, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// ListEvents pages the device audit trail (GET /api/v1/events).
// limit <= 0 means the server default; afterID > 0 pages forward from an
// event id; kname filters to one device.
func (c *Client) ListEvents(ctx context.Context, limit int, afterID uint64, kname string) (*Events, error) {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if afterID > 0 {
		q.Set("after_id", strconv.FormatUint(afterID, 10))
	}
	if kname != "" {
		q.Set("kname", kname)
	}
	path := "/api/v1/events"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var e Events
	if err := c.getJSON(ctx, path, &e); err != nil {
		return nil, err
	}
	if e.Events == nil {
		e.Events = []DeviceEvent{}
	}
	return &e, nil
}

// WatchEvents tails the live device event stream (SSE at
// /api/v1/events/stream). It returns a channel fed as events arrive; the
// channel closes when the stream ends (cancel ctx to disconnect). Each SSE
// record carries the same fields as ListEvents entries.
func (c *Client) WatchEvents(ctx context.Context) (<-chan DeviceEvent, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+"/api/v1/events/stream", nil)
	if err != nil {
		return nil, &Error{Err: err}
	}
	req.Header.Set("Accept", "text/event-stream")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	// Streaming needs no overall timeout; ctx cancellation controls lifetime.
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return nil, &Error{Err: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		var env struct {
			Error *APIError `json:"error"`
		}
		if json.Unmarshal(body, &env) == nil && env.Error != nil {
			return nil, env.Error
		}
		return nil, &Error{Err: fmt.Errorf("HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(body))}
	}

	ch := make(chan DeviceEvent, 64)
	go func() {
		defer resp.Body.Close()
		defer close(ch)
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if !strings.HasPrefix(line, "data: ") {
				continue // :connected comment, keepalives, blank lines
			}
			var ev DeviceEvent
			if err := json.Unmarshal([]byte(line[6:]), &ev); err != nil {
				continue
			}
			select {
			case ch <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

// --- plumbing ---

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	return c.doJSON(ctx, http.MethodGet, path, nil, out)
}

func (c *Client) delete(ctx context.Context, path string) error {
	return c.doJSON(ctx, http.MethodDelete, path, nil, nil)
}

func (c *Client) doJSON(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return &Error{Err: err}
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, body)
	if err != nil {
		return &Error{Err: err}
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return &Error{Err: err}
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return &Error{Err: err}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var env struct {
			Error *APIError `json:"error"`
		}
		if json.Unmarshal(bodyBytes, &env) == nil && env.Error != nil {
			return env.Error
		}
		return &Error{Err: fmt.Errorf("HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(bodyBytes))}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(bodyBytes, out); err != nil {
		return &Error{Err: fmt.Errorf("decode %s: %w", path, err)}
	}

	// protojson omits empty repeated fields, which unmarshal to nil slices;
	// normalize so callers always see [] instead of null (lists are lists).
	switch o := out.(type) {
	case *Pools:
		if o.Pools == nil {
			o.Pools = []Pool{}
		}
	case *Shares:
		if o.Shares == nil {
			o.Shares = []Share{}
		}
	case *Devices:
		if o.Devices == nil {
			o.Devices = []Device{}
		}
	}
	return nil
}
