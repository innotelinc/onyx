package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
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

func (s *server) handleUsers(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"users": s.users.list()})
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

func (s *server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	s.users.mu.Lock()
	defer s.users.mu.Unlock()
	if _, ok := s.users.users[r.PathValue("id")]; !ok {
		writeEnvelope(w, 404, apiError{Code: "not_found", Message: "user not found"})
		return
	}
	delete(s.users.users, r.PathValue("id"))
	if err := s.users.persistLocked(); err != nil {
		writeEnvelope(w, 500, apiError{Code: "internal", Message: err.Error()})
		return
	}
	writeJSON(w, 200, map[string]bool{"deleted": true})
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
