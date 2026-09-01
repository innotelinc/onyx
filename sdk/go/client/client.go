// Package client is the onyx-sdk Go client. It talks to the onyx-api gateway
// over its REST surface (docs/design/06), so scripts and apps interact with
// Onyx exactly like the UI does.
package client

import (
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

// Pools is the response of GET /api/v1/pools.
type Pools struct {
	Pools []Pool `json:"pools"`
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

// --- plumbing ---

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+path, nil)
	if err != nil {
		return &Error{Err: err}
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return &Error{Err: err}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return &Error{Err: err}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var env struct {
			Error *APIError `json:"error"`
		}
		if json.Unmarshal(body, &env) == nil && env.Error != nil {
			return env.Error
		}
		return &Error{Err: fmt.Errorf("HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(body))}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return &Error{Err: fmt.Errorf("decode %s: %w", path, err)}
	}

	// protojson omits empty repeated fields, which unmarshal to nil slices;
	// normalize so callers always see [] instead of null (lists are lists).
	if p, ok := out.(*Pools); ok && p.Pools == nil {
		p.Pools = []Pool{}
	}
	return nil
}