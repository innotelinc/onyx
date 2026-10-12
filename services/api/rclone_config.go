package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// rclone's non-interactive protocol carries provider questions such as Apple's
// verification code and Microsoft's drive/library selection. Keep its state
// server-side so the browser cannot invent arbitrary backend transitions.
type remoteConfigQuestion struct {
	Name       string `json:"name"`
	Help       string `json:"help"`
	Default    any    `json:"default,omitempty"`
	Required   bool   `json:"required"`
	IsPassword bool   `json:"is_password"`
	Exclusive  bool   `json:"exclusive"`
	Examples   []struct {
		Value string `json:"Value"`
		Help  string `json:"Help"`
	} `json:"examples,omitempty"`
}

type remoteConfigStep struct {
	ID       string               `json:"id"`
	Question remoteConfigQuestion `json:"question"`
	Error    string               `json:"error,omitempty"`
	state    string
	created  time.Time
}

type remoteConfigStore struct {
	sync.Mutex
	steps map[string]remoteConfigStep
}

var pendingRemoteConfigs = remoteConfigStore{steps: map[string]remoteConfigStep{}}

func (s *remoteConfigStore) capture(name, output string) *remoteConfigStep {
	var result struct {
		State  string
		Option *struct {
			Name, Help                      string
			Default                         any
			Required, IsPassword, Exclusive bool
			Examples                        []struct{ Value, Help string }
		}
		Error string
	}
	if json.Unmarshal([]byte(output), &result) != nil {
		return nil
	}
	s.Lock()
	defer s.Unlock()
	delete(s.steps, name)
	if result.State == "" || result.Option == nil {
		return nil
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil
	}
	step := remoteConfigStep{ID: hex.EncodeToString(nonce[:]), state: result.State, created: time.Now(), Error: result.Error}
	step.Question.Name, step.Question.Help = result.Option.Name, result.Option.Help
	step.Question.Required, step.Question.IsPassword, step.Question.Exclusive = result.Option.Required, result.Option.IsPassword, result.Option.Exclusive
	if !result.Option.IsPassword {
		step.Question.Default = result.Option.Default
	}
	for _, example := range result.Option.Examples {
		step.Question.Examples = append(step.Question.Examples, struct {
			Value string `json:"Value"`
			Help  string `json:"Help"`
		}{example.Value, example.Help})
	}
	s.steps[name] = step
	return &step
}

func (s *remoteConfigStore) all() map[string]remoteConfigStep {
	s.Lock()
	defer s.Unlock()
	result := map[string]remoteConfigStep{}
	for name, step := range s.steps {
		if time.Since(step.created) > oauthStateTTL {
			delete(s.steps, name)
			continue
		}
		result[name] = step
	}
	return result
}

func (s *server) handleRemoteConfiguration(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if validateRemoteName(name) != nil {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "Invalid account name"})
		return
	}
	var body struct{ ID, Result string }
	r.Body = http.MaxBytesReader(w, r.Body, 128*1024)
	if json.NewDecoder(r.Body).Decode(&body) != nil || len(body.Result) > 64*1024 || strings.ContainsRune(body.Result, 0) {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "Invalid setup answer"})
		return
	}
	pendingRemoteConfigs.Lock()
	step, ok := pendingRemoteConfigs.steps[name]
	if !ok || step.ID != body.ID || time.Since(step.created) > oauthStateTTL {
		pendingRemoteConfigs.Unlock()
		writeEnvelope(w, http.StatusConflict, apiError{Code: "failed_precondition", Message: "This setup step expired. Reconnect the account to start again."})
		return
	}
	if step.Question.Required && strings.TrimSpace(body.Result) == "" {
		pendingRemoteConfigs.Unlock()
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "An answer is required"})
		return
	}
	if step.Question.Exclusive && len(step.Question.Examples) > 0 {
		valid := false
		for _, example := range step.Question.Examples {
			if example.Value == body.Result {
				valid = true
				break
			}
		}
		if !valid {
			pendingRemoteConfigs.Unlock()
			writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "Choose one of the provider's listed options"})
			return
		}
	}
	delete(pendingRemoteConfigs.steps, name) // one request owns this transition
	pendingRemoteConfigs.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	output, err := runRclone(ctx, append([]string{"config", "update", name, "--non-interactive", "--continue", "--state", step.state, "--result", body.Result}, networkBounds...)...)
	if err != nil {
		// Allow correction of a rejected verification code without exposing it.
		pendingRemoteConfigs.Lock()
		pendingRemoteConfigs.steps[name] = step
		pendingRemoteConfigs.Unlock()
		writeEnvelope(w, http.StatusBadGateway, apiError{Code: "failed_precondition", Message: "The provider rejected this setup answer. Check the answer and retry."})
		return
	}
	if next := pendingRemoteConfigs.capture(name, output); next != nil {
		writeJSON(w, http.StatusOK, map[string]any{"name": name, "configuration": next})
		return
	}
	output, err = runRclone(ctx, append([]string{"lsd", name + ":", "--max-depth", "1"}, networkBounds...)...)
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "ok": err == nil, "detail": output})
}

func (s *server) handleReconnectRemote(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if validateRemoteName(name) != nil {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "Invalid account name"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	opts, err := remoteOptions(ctx, name)
	if err != nil {
		writeEnvelope(w, http.StatusNotFound, apiError{Code: "not_found", Message: "Account not found"})
		return
	}
	if opts["type"] != "iclouddrive" {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "Use Connect to authorize this provider"})
		return
	}
	output, err := runRclone(ctx, append([]string{"config", "update", name, "--non-interactive"}, networkBounds...)...)
	if err != nil {
		writeEnvelope(w, http.StatusBadGateway, apiError{Code: "failed_precondition", Message: "Could not reconnect iCloud. Check your Apple Account password, web access and network connection."})
		return
	}
	if step := pendingRemoteConfigs.capture(name, output); step != nil {
		writeJSON(w, http.StatusOK, map[string]any{"name": name, "configuration": step})
		return
	}
	output, err = runRclone(ctx, append([]string{"lsd", name + ":", "--max-depth", "1"}, networkBounds...)...)
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "ok": err == nil, "detail": output})
}
