package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	onyxv1 "github.com/innotelinc/onyx/proto/gen/go/onyx/v1"
)

// The access surface: who can reach a share, what their name can actually sign
// in with, and what has been granted or refused. All three are answered by
// onyx-core, which records the grants and renders them into the share backends,
// so the console never shows a permission the daemons do not have.

// handleShareAccess serves GET /api/v1/shares/{name}/access — the grants on one
// share, which is the view the Shares page edits.
func (s *server) handleShareAccess(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	resp, err := s.coreShares.ListShareAccess(ctx, &onyxv1.ListShareAccessRequest{Share: name})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"share":  name,
		"access": orEmptyAccess(resp.GetAccess()),
		// Samba only admits an account that exists, so the console needs to know
		// which grantees can actually sign in over SMB.
		"samba_accounts": s.sambaAccounts(ctx),
	})
}

// orEmptyAccess keeps "nobody is granted this share" a list rather than null, so
// a client iterating it does not have to special-case the empty share.
func orEmptyAccess(access []*onyxv1.ShareAccess) []*onyxv1.ShareAccess {
	if access == nil {
		return []*onyxv1.ShareAccess{}
	}
	return access
}

// handleSetShareAccess serves PUT /api/v1/shares/{name}/access. The body
// carries the complete desired map for the share (username -> "read" |
// "read-write" | "" to remove), and only what changed is written: every write
// re-renders the daemon config the backends serve.
func (s *server) handleSetShareAccess(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var body struct {
		Access map[string]string `json:"access"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeEnvelope(w, 400, apiError{Code: "invalid_argument", Message: err.Error()})
		return
	}
	for user, mode := range body.Access {
		if mode != "" && mode != "read" && mode != "read-write" {
			writeEnvelope(w, 400, apiError{Code: "invalid_argument", Message: "permission must be read or read-write for " + user})
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	current, err := s.coreShares.ListShareAccess(ctx, &onyxv1.ListShareAccessRequest{Share: name})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	existing := map[string]string{}
	for _, a := range current.GetAccess() {
		existing[a.GetUsername()] = a.GetMode()
	}
	actor := requestActor(r)
	for _, username := range unionShares(existing, body.Access) {
		want := body.Access[username]
		if existing[username] == want {
			continue
		}
		if _, err := s.coreShares.SetShareAccess(ctx, &onyxv1.SetShareAccessRequest{
			Access: &onyxv1.ShareAccess{Share: name, Username: username, Mode: want},
			Actor:  actor,
		}); err != nil {
			s.writeGRPCError(w, r, err)
			return
		}
	}
	resp, err := s.coreShares.ListShareAccess(ctx, &onyxv1.ListShareAccessRequest{Share: name})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"share":          name,
		"access":         orEmptyAccess(resp.GetAccess()),
		"samba_accounts": s.sambaAccounts(ctx),
	})
}

// requestActor names whoever made a change for the audit trail. The gateway
// sees the operator's identity as a header when it is behind the authenticating
// proxy; a direct call has none, and core records that as "console".
func requestActor(r *http.Request) string {
	for _, header := range []string{"X-Onyx-User", "X-Authentik-Username", "X-Forwarded-User"} {
		if value := strings.TrimSpace(r.Header.Get(header)); value != "" {
			return value
		}
	}
	return ""
}

// handleSambaAccounts serves GET /api/v1/samba/accounts — which Onyx users have
// an account Samba will accept.
func (s *server) handleSambaAccounts(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	accounts, err := s.sambaAccountsRaw(ctx)
	if err != nil {
		writeJSON(w, 200, map[string]any{"available": false, "reason": err.Error(), "accounts": []string{}})
		return
	}
	writeJSON(w, 200, map[string]any{"available": true, "accounts": accounts})
}

// sambaAccounts is the list for the console, empty when Samba is not part of
// this deployment (a container with no samba has no passdb to read).
func (s *server) sambaAccounts(ctx context.Context) []string {
	accounts, err := s.sambaAccountsRaw(ctx)
	if err != nil {
		return []string{}
	}
	return accounts
}

func (s *server) sambaAccountsRaw(ctx context.Context) ([]string, error) {
	if s.core == nil {
		return nil, fmt.Errorf("onyx-core is not reachable")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	resp, err := s.core.ProvisionSambaUser(ctx, &onyxv1.ProvisionSambaUserRequest{Action: "list"})
	if err != nil {
		return nil, err
	}
	accounts := append([]string{}, resp.GetAccounts()...)
	sort.Strings(accounts)
	return accounts, nil
}

type sambaPasswordBody struct {
	Password string `json:"password"`
}

// handleSetSambaPassword serves POST /api/v1/users/{id}/smb-password: it creates
// (or re-passwords) the Samba account for one Onyx user, so a share that grants
// them can be reached over SMB.
func (s *server) handleSetSambaPassword(w http.ResponseWriter, r *http.Request) {
	u, ok := s.getUser(r.PathValue("id"))
	if !ok {
		writeEnvelope(w, 404, apiError{Code: "not_found", Message: "user not found"})
		return
	}
	var body sambaPasswordBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeEnvelope(w, 400, apiError{Code: "invalid_argument", Message: err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	resp, err := s.core.ProvisionSambaUser(ctx, &onyxv1.ProvisionSambaUserRequest{
		Username: u.Username,
		Action:   "add",
		Password: body.Password,
	})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"user_id":        u.ID,
		"username":       u.Username,
		"provisioned":    resp.GetProvisioned(),
		"samba_accounts": resp.GetAccounts(),
		// The password is never echoed back, not even a masked form of it.
		"message": "The account can now sign in to SMB shares it is granted.",
	})
}

// handleDeleteSambaPassword serves DELETE /api/v1/users/{id}/smb-password: the
// account is removed, so SMB stops accepting that name at all.
func (s *server) handleDeleteSambaPassword(w http.ResponseWriter, r *http.Request) {
	u, ok := s.getUser(r.PathValue("id"))
	if !ok {
		writeEnvelope(w, 404, apiError{Code: "not_found", Message: "user not found"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	resp, err := s.core.ProvisionSambaUser(ctx, &onyxv1.ProvisionSambaUserRequest{
		Username: u.Username,
		Action:   "remove",
	})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"user_id":        u.ID,
		"username":       u.Username,
		"provisioned":    resp.GetProvisioned(),
		"samba_accounts": resp.GetAccounts(),
	})
}

// handleAccessAudit serves GET /api/v1/audit/access — the grants that were
// changed and the requests that were refused, newest first.
func (s *server) handleAccessAudit(w http.ResponseWriter, r *http.Request) {
	limit := 0
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeEnvelope(w, 400, apiError{Code: "invalid_argument", Message: "limit must be a non-negative integer"})
			return
		}
		limit = n
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	resp, err := s.core.ListAccessEvents(ctx, &onyxv1.ListAccessEventsRequest{
		Share: strings.TrimSpace(r.URL.Query().Get("share")),
		Limit: int32(limit),
	})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"events": resp.GetEvents()})
}
