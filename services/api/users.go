package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var validRoles = map[string]bool{"admin": true, "operator": true, "user": true, "viewer": true}

type onyxUser struct {
	ID          string            `json:"id"`
	Username    string            `json:"username"`
	DisplayName string            `json:"display_name"`
	Email       string            `json:"email,omitempty"`
	Role        string            `json:"role"`
	Status      string            `json:"status"`
	Permissions map[string]string `json:"permissions,omitempty"`
	CreatedAt   string            `json:"created_at"`
	// Source says where the account came from: an Onyx-only mapping, an
	// Authentik account, or both. InAuthentik/Active are the identity
	// provider's own answer, so the console can show a locked account as locked
	// instead of guessing from the local status.
	Source      string `json:"source,omitempty"`
	InAuthentik bool   `json:"in_authentik,omitempty"`
	Active      bool   `json:"active,omitempty"`
}

type userStore struct {
	mu     sync.RWMutex
	path   string
	users  map[string]*onyxUser
	nextID uint64
}

func newUserStore(stateDir string) (*userStore, error) {
	s := &userStore{path: filepath.Join(stateDir, "users.json"), users: map[string]*onyxUser{}}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.users); err != nil {
		return nil, fmt.Errorf("decode users: %w", err)
	}
	s.nextID = uint64(time.Now().UnixNano())
	return s, nil
}

func (s *userStore) persistLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o750); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s.users, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *userStore) list() []*onyxUser {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*onyxUser, 0, len(s.users))
	for _, u := range s.users {
		cp := *u
		out = append(out, &cp)
	}
	return out
}

// upsert finds a user by username (case-insensitively) or creates one, applies
// mut to it, and persists. This is how an Authentik account gets an Onyx role
// on first sight without a second store to keep in step.
func (s *userStore) upsert(username string, mut func(*onyxUser)) (*onyxUser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.users {
		if strings.EqualFold(u.Username, username) {
			mut(u)
			if err := s.persistLocked(); err != nil {
				return nil, err
			}
			cp := *u
			return &cp, nil
		}
	}
	s.nextID++
	u := &onyxUser{
		ID: fmt.Sprintf("usr_%d", s.nextID), Username: username, Role: "user", Status: "active",
		Permissions: map[string]string{}, CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	mut(u)
	if u.Role == "" {
		u.Role = "user"
	}
	if u.Status == "" {
		u.Status = "active"
	}
	s.users[u.ID] = u
	if err := s.persistLocked(); err != nil {
		delete(s.users, u.ID)
		return nil, err
	}
	cp := *u
	return &cp, nil
}

// handleUsers merges the two halves of a user: the role Onyx stores and the
// identity Authentik owns. An Authentik account with no Onyx role gets one on
// first sight, so the two never drift apart and nobody has to remember to add
// the mapping by hand.
func (s *server) handleUsers(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	merged := map[string]*onyxUser{}
	for _, u := range s.users.list() {
		cp := *u
		if cp.Source == "" {
			cp.Source = "onyx"
		}
		merged[strings.ToLower(u.Username)] = &cp
	}
	sync := map[string]any{"enabled": s.authentik.enabled(), "reason": s.authentik.reason()}
	if s.authentik.enabled() {
		accounts, err := s.authentik.listUsers(ctx)
		if err != nil {
			sync["error"] = err.Error()
		} else {
			sync["authentik_users"] = len(accounts)
			for _, account := range accounts {
				key := strings.ToLower(account.Username)
				if existing, ok := merged[key]; ok {
					existing.Source = "authentik+onyx"
					existing.InAuthentik = true
					existing.Active = account.IsActive
					if account.Name != "" {
						existing.DisplayName = account.Name
					}
					if account.Email != "" {
						existing.Email = account.Email
					}
					continue
				}
				created, err := s.users.upsert(account.Username, func(u *onyxUser) {
					u.DisplayName = account.Name
					u.Email = account.Email
					if account.IsActive {
						u.Status = "active"
					} else {
						u.Status = "locked"
					}
				})
				if err != nil {
					continue
				}
				cp := *created
				cp.Source = "authentik"
				cp.InAuthentik = true
				cp.Active = account.IsActive
				merged[key] = &cp
			}
		}
	}
	users := make([]*onyxUser, 0, len(merged))
	for _, u := range merged {
		users = append(users, u)
	}
	sort.Slice(users, func(i, j int) bool { return strings.ToLower(users[i].Username) < strings.ToLower(users[j].Username) })
	writeJSON(w, http.StatusOK, map[string]any{"users": users, "authentik": sync})
}

type userBody struct {
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
	Role        string `json:"role"`
	Status      string `json:"status"`
}

func validateUserBody(body userBody) error {
	body.Username = strings.TrimSpace(body.Username)
	if body.Username == "" {
		return errors.New("username is required")
	}
	if body.Role != "" && !validRoles[body.Role] {
		return fmt.Errorf("unknown role %q", body.Role)
	}
	if body.Status != "" && body.Status != "active" && body.Status != "locked" && body.Status != "pending" {
		return fmt.Errorf("unknown status %q", body.Status)
	}
	return nil
}

func (s *server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var body userBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeEnvelope(w, 400, apiError{Code: "invalid_argument", Message: err.Error()})
		return
	}
	if err := validateUserBody(body); err != nil {
		writeEnvelope(w, 400, apiError{Code: "invalid_argument", Message: err.Error()})
		return
	}
	// The account is created in Authentik first: identity lives there, and a
	// mapping for somebody who cannot sign in would be a lie the console tells.
	if s.authentik.enabled() {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		account, err := s.authentik.findUser(ctx, strings.TrimSpace(body.Username))
		if err != nil {
			writeEnvelope(w, 502, apiError{Code: "unavailable", Message: "Authentik could not be read: " + err.Error()})
			return
		}
		if account == nil {
			name := body.DisplayName
			if name == "" {
				name = strings.TrimSpace(body.Username)
			}
			if _, err := s.authentik.createUser(ctx, strings.TrimSpace(body.Username), name, body.Email); err != nil {
				writeEnvelope(w, 502, apiError{Code: "unavailable", Message: err.Error()})
				return
			}
		}
	}
	s.users.mu.Lock()
	defer s.users.mu.Unlock()
	for _, u := range s.users.users {
		if u.Username == body.Username {
			writeEnvelope(w, 409, apiError{Code: "already_exists", Message: "username already exists"})
			return
		}
	}
	s.users.nextID++
	u := &onyxUser{ID: fmt.Sprintf("usr_%d", s.users.nextID), Username: strings.TrimSpace(body.Username), DisplayName: body.DisplayName, Email: body.Email, Role: body.Role, Status: body.Status, Permissions: map[string]string{}, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	if u.Role == "" {
		u.Role = "user"
	}
	if u.Status == "" {
		u.Status = "active"
	}
	s.users.users[u.ID] = u
	if err := s.users.persistLocked(); err != nil {
		delete(s.users.users, u.ID)
		writeEnvelope(w, 500, apiError{Code: "internal", Message: err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, u)
}

func (s *server) getUser(id string) (*onyxUser, bool) {
	s.users.mu.RLock()
	defer s.users.mu.RUnlock()
	u, ok := s.users.users[id]
	if !ok {
		return nil, false
	}
	cp := *u
	return &cp, true
}

// getUserByUsername finds a mapping by username, which is the key Authentik
// knows an account by (the local id is generated here and means nothing there).
func (s *server) getUserByUsername(username string) (*onyxUser, bool) {
	for _, u := range s.users.list() {
		if strings.EqualFold(u.Username, username) {
			return u, true
		}
	}
	return nil, false
}

func (s *server) handleUser(w http.ResponseWriter, r *http.Request) {
	u, ok := s.getUser(r.PathValue("id"))
	if !ok {
		writeEnvelope(w, 404, apiError{Code: "not_found", Message: "user not found"})
		return
	}
	writeJSON(w, 200, u)
}

func (s *server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	var body userBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeEnvelope(w, 400, apiError{Code: "invalid_argument", Message: err.Error()})
		return
	}
	if body.Role != "" && !validRoles[body.Role] {
		writeEnvelope(w, 400, apiError{Code: "invalid_argument", Message: "invalid role"})
		return
	}
	s.users.mu.Lock()
	defer s.users.mu.Unlock()
	u, ok := s.users.users[r.PathValue("id")]
	if !ok {
		writeEnvelope(w, 404, apiError{Code: "not_found", Message: "user not found"})
		return
	}
	if body.DisplayName != "" {
		u.DisplayName = body.DisplayName
	}
	if body.Email != "" {
		u.Email = body.Email
	}
	if body.Role != "" {
		u.Role = body.Role
	}
	if body.Status != "" {
		u.Status = body.Status
	}
	if err := s.users.persistLocked(); err != nil {
		writeEnvelope(w, 500, apiError{Code: "internal", Message: err.Error()})
		return
	}
	writeJSON(w, 200, u)
}

// handleDeleteUser removes the Onyx role mapping. The Authentik account behind
// it is disabled rather than deleted, so its history, groups and grants survive
// — unless the caller asks to purge, which is the only path that erases an
// identity, and then it is erased in both places.
func (s *server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	user, ok := s.getUser(id)
	if !ok {
		writeEnvelope(w, 404, apiError{Code: "not_found", Message: "user not found"})
		return
	}
	purge := strings.EqualFold(r.URL.Query().Get("purge"), "true")
	authentik := "skipped"
	if !s.authentik.enabled() {
		authentik = s.authentik.reason()
	} else {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		account, err := s.authentik.findUser(ctx, user.Username)
		switch {
		case err != nil:
			authentik = "could not be read: " + err.Error()
		case account == nil:
			authentik = "no such account"
		case purge:
			if err := s.authentik.deleteUser(ctx, account.PK); err != nil {
				authentik = err.Error()
			} else {
				authentik = "deleted"
			}
		default:
			if err := s.authentik.setActive(ctx, account.PK, false); err != nil {
				authentik = err.Error()
			} else {
				authentik = "disabled"
			}
		}
	}
	s.users.mu.Lock()
	defer s.users.mu.Unlock()
	delete(s.users.users, id)
	if err := s.users.persistLocked(); err != nil {
		writeEnvelope(w, 500, apiError{Code: "internal", Message: err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": true, "authentik": authentik})
}

func (s *server) handleUserPermissions(w http.ResponseWriter, r *http.Request) {
	u, ok := s.getUser(r.PathValue("id"))
	if !ok {
		writeEnvelope(w, 404, apiError{Code: "not_found", Message: "user not found"})
		return
	}
	writeJSON(w, 200, map[string]any{"user_id": u.ID, "permissions": u.Permissions})
}

func (s *server) handleSetUserPermissions(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Permissions map[string]string `json:"permissions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeEnvelope(w, 400, apiError{Code: "invalid_argument", Message: err.Error()})
		return
	}
	s.users.mu.Lock()
	defer s.users.mu.Unlock()
	u, ok := s.users.users[r.PathValue("id")]
	if !ok {
		writeEnvelope(w, 404, apiError{Code: "not_found", Message: "user not found"})
		return
	}
	for share, mode := range body.Permissions {
		if mode != "read" && mode != "read-write" {
			writeEnvelope(w, 400, apiError{Code: "invalid_argument", Message: "permission must be read or read-write for " + share})
			return
		}
	}
	u.Permissions = body.Permissions
	if err := s.users.persistLocked(); err != nil {
		writeEnvelope(w, 500, apiError{Code: "internal", Message: err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"user_id": u.ID, "permissions": u.Permissions})
}
