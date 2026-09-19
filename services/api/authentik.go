package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Authentik owns identity (docs/design/11 §2): Onyx stores only the local role
// and storage grants. The Users page has to reflect both halves at once, so this
// is the client that reads and writes accounts in Authentik and the merge that
// presents them beside their Onyx role.
//
// Writes go to Authentik first and the local mapping only afterwards: creating a
// mapping for an account that Authentik rejected would leave a user who cannot
// sign in but is listed as if they could.

// authentikClient talks to Authentik's core API using a service token.
type authentikClient struct {
	baseURL string
	token   string
	http    *http.Client
}

// authentikFromEnv builds the client from the deployment's environment.
// AUTHENTIK_TOKEN is preferred; the bootstrap token is accepted as a fallback
// because that is what setup.sh already mints.
func authentikFromEnv() *authentikClient {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("AUTHENTIK_URL")), "/")
	token := strings.TrimSpace(os.Getenv("AUTHENTIK_TOKEN"))
	if token == "" {
		token = strings.TrimSpace(os.Getenv("AUTHENTIK_BOOTSTRAP_TOKEN"))
	}
	return &authentikClient{
		// The compose default points at the bundled instance.
		baseURL: base,
		token:   token,
		http:    &http.Client{Timeout: 20 * time.Second},
	}
}

func (c *authentikClient) enabled() bool { return c != nil && c.baseURL != "" && c.token != "" }

// reason explains why sync is off, so the console can say something useful
// instead of quietly showing a local-only list.
func (c *authentikClient) reason() string {
	if c == nil || c.baseURL == "" {
		return "AUTHENTIK_URL is not set, so Onyx cannot reach Authentik."
	}
	if c.token == "" {
		return "AUTHENTIK_TOKEN is not set, so Onyx can read but not change Authentik users."
	}
	return ""
}

func (c *authentikClient) do(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		payload = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, payload)
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, response.StatusCode, err
	}
	return raw, response.StatusCode, nil
}

// authentikUser is the part of Authentik's user record Onyx cares about.
type authentikUser struct {
	PK       int    `json:"pk"`
	Username string `json:"username"`
	Name     string `json:"name"`
	Email    string `json:"email"`
	IsActive bool   `json:"is_active"`
	Path     string `json:"path"`
	Type     string `json:"type"`
}

type authentikUserPage struct {
	Pagination struct {
		Next    string `json:"next"`
		Count   int    `json:"count"`
		Total   int    `json:"total_pages"`
		Current int    `json:"current_page"`
	} `json:"pagination"`
	Results []authentikUser `json:"results"`
}

// listUsers reads every account Authentik has, following its pagination.
func (c *authentikClient) listUsers(ctx context.Context) ([]authentikUser, error) {
	if !c.enabled() {
		return nil, fmt.Errorf("%s", c.reason())
	}
	out := []authentikUser{}
	path := "/api/v3/core/users/?page_size=100&ordering=username"
	for pages := 0; path != "" && pages < 50; pages++ {
		raw, status, err := c.do(ctx, http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("Authentik returned %d for the user list: %s", status, firstLine(raw))
		}
		var page authentikUserPage
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, fmt.Errorf("could not read Authentik's user list: %w", err)
		}
		out = append(out, page.Results...)
		// `next` is an absolute URL; keep only the path so the base URL stays
		// the one this deployment configured.
		if next, err := url.Parse(page.Pagination.Next); err == nil && next.Path != "" {
			path = next.Path + "?" + next.RawQuery
		} else {
			path = ""
		}
	}
	return out, nil
}

// findUser looks an account up by username (Authentik's own unique key here).
func (c *authentikClient) findUser(ctx context.Context, username string) (*authentikUser, error) {
	raw, status, err := c.do(ctx, http.MethodGet, "/api/v3/core/users/?username="+url.QueryEscape(username), nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("Authentik returned %d looking up %q: %s", status, username, firstLine(raw))
	}
	var page authentikUserPage
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, err
	}
	for i := range page.Results {
		if strings.EqualFold(page.Results[i].Username, username) {
			return &page.Results[i], nil
		}
	}
	return nil, nil
}

// createUser adds an account. Authentik creates it without a password: the
// person sets one through its recovery flow, which is the only way a password
// should ever be minted.
func (c *authentikClient) createUser(ctx context.Context, username, name, email string) (*authentikUser, error) {
	if !c.enabled() {
		return nil, fmt.Errorf("%s", c.reason())
	}
	if name == "" {
		name = username
	}
	raw, status, err := c.do(ctx, http.MethodPost, "/api/v3/core/users/", map[string]any{
		"username":  username,
		"name":      name,
		"email":     email,
		"is_active": true,
		"path":      "users",
		"type":      "internal",
	})
	if err != nil {
		return nil, err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return nil, fmt.Errorf("Authentik refused to create %q (%d): %s", username, status, firstLine(raw))
	}
	var created authentikUser
	if err := json.Unmarshal(raw, &created); err != nil {
		return nil, fmt.Errorf("could not read the created Authentik user: %w", err)
	}
	return &created, nil
}

// setActive enables or disables an account. Disabling is what removing a user
// from the console does: the identity stays in Authentik (so its history and
// group memberships survive) but it can no longer sign in.
func (c *authentikClient) setActive(ctx context.Context, pk int, active bool) error {
	if !c.enabled() {
		return fmt.Errorf("%s", c.reason())
	}
	raw, status, err := c.do(ctx, http.MethodPatch, fmt.Sprintf("/api/v3/core/users/%d/", pk), map[string]any{"is_active": active})
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusNoContent {
		return fmt.Errorf("Authentik returned %d changing the account: %s", status, firstLine(raw))
	}
	return nil
}

// deleteUser removes the account entirely. Only a deliberate purge does this.
func (c *authentikClient) deleteUser(ctx context.Context, pk int) error {
	if !c.enabled() {
		return fmt.Errorf("%s", c.reason())
	}
	raw, status, err := c.do(ctx, http.MethodDelete, fmt.Sprintf("/api/v3/core/users/%d/", pk), nil)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent && status != http.StatusOK {
		return fmt.Errorf("Authentik returned %d deleting the account: %s", status, firstLine(raw))
	}
	return nil
}

func firstLine(raw []byte) string {
	text := strings.TrimSpace(string(raw))
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		text = text[:index]
	}
	if len(text) > 300 {
		text = text[:300] + "…"
	}
	return text
}
