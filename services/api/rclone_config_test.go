package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGoogleCloudRegistrationAcceptsUploadedJSON(t *testing.T) {
	log := fakeRclone(t)
	key := "{\n\"type\":\"service_account\",\n\"project_id\":\"test-project\",\n\"client_email\":\"test@example.iam.gserviceaccount.com\",\n\"private_key\":\"-----BEGIN PRIVATE KEY-----\\nTEST\\n-----END PRIVATE KEY-----\\n\"\n}"
	body, _ := json.Marshal(createRemoteBody{Name: "google-buckets", Type: "google cloud storage", Params: map[string]string{"service_account_credentials": key}})
	rec := post(t, (&server{}).handleCreateRemote, "/api/v1/storage/remotes", string(body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	args, _ := os.ReadFile(log)
	if !strings.Contains(string(args), `service_account_credentials {"type":"service_account"`) {
		t.Fatalf("JSON was not compacted and handed to rclone")
	}
	if strings.Contains(rec.Body.String(), "PRIVATE KEY") || strings.Contains(rec.Body.String(), "client_email") {
		t.Fatal("credentials leaked in the response")
	}
}

func TestGoogleCloudRegistrationRejectsMissingOrInvalidKeys(t *testing.T) {
	log := fakeRclone(t)
	for _, params := range []map[string]string{
		{}, {"service_account_credentials": "not JSON"}, {"service_account_credentials": `{"type":"authorized_user"}`},
		{"service_account_credentials": `{"type":"service_account"}`, "service_account_file": "/keys/key.json"},
	} {
		body, _ := json.Marshal(createRemoteBody{Name: "gcs", Type: "google cloud storage", Params: params})
		rec := post(t, (&server{}).handleCreateRemote, "/api/v1/storage/remotes", string(body))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status %d, want 400", rec.Code)
		}
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatal("invalid credentials reached rclone")
	}
	body, _ := json.Marshal(createRemoteBody{Name: "gcs", Type: "google cloud storage", Params: map[string]string{"service_account_file": "/etc/rclone/key.json"}})
	if rec := post(t, (&server{}).handleCreateRemote, "/api/v1/storage/remotes", string(body)); rec.Code != http.StatusCreated {
		t.Fatalf("existing key-file path rejected: %s", rec.Body.String())
	}
}

func TestCreateRemoteDoesNotOverwriteAnExistingAccount(t *testing.T) {
	log := fakeRcloneWith(t, "existing:")
	rec := post(t, (&server{}).handleCreateRemote, "/api/v1/storage/remotes", `{"name":"existing","type":"drive"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409", rec.Code)
	}
	args, _ := os.ReadFile(log)
	if strings.Contains(string(args), "config create") {
		t.Fatal("existing account overwritten")
	}
}

func TestSharePointUsesTheOneDriveBackend(t *testing.T) {
	log := fakeRclone(t)
	rec := post(t, (&server{}).handleCreateRemote, "/api/v1/storage/remotes", `{"name":"team","type":"sharepoint"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	args, _ := os.ReadFile(log)
	if !strings.Contains(string(args), "config create team onedrive") || !strings.Contains(string(args), "config_is_local false") {
		t.Fatalf("SharePoint did not use Microsoft's headless setup: %s", args)
	}
}

// Return real protocol-shaped questions without talking to a cloud account.
func fakeRcloneQuestion(t *testing.T, initial string) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + log + "'\nif [ \"$1\" = listremotes ]; then echo 'icloud-test:'; exit; fi\nif [ \"$1\" = config ] && [ \"$2\" = create ]; then cat <<'JSON'\n" + initial + "\nJSON\nelif [ \"$1\" = config ] && [ \"$2\" = update ]; then echo '{\"State\":\"\"}'; fi\n"
	if err := os.WriteFile(filepath.Join(dir, "rclone"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

const appleQuestion = `{"State":"2fa_do","Option":{"Name":"config_2fa","Help":"Enter the Apple verification code","IsPassword":true,"Required":true,"Default":"never-show-this"}}`

func TestConfigurationContinuesProviderTwoFactorAuthentication(t *testing.T) {
	log := fakeRcloneQuestion(t, appleQuestion)
	name := "icloud-2fa"
	rec := post(t, (&server{}).handleCreateRemote, "/api/v1/storage/remotes", `{"name":"icloud-2fa","type":"iclouddrive","params":{"apple_id":"test@example.com","password":"example-password"}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var result struct{ Configuration remoteConfigStep }
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Configuration.ID == "" || strings.Contains(rec.Body.String(), "2fa_do") || strings.Contains(rec.Body.String(), "never-show-this") {
		t.Fatal("missing challenge or sensitive state exposed")
	}
	submit := func(id string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"id": id, "result": "123456"})
		req := httptest.NewRequest(http.MethodPost, "/configure", bytes.NewReader(body))
		req.SetPathValue("name", name)
		answer := httptest.NewRecorder()
		(&server{}).handleRemoteConfiguration(answer, req)
		return answer
	}
	if bad := submit("forged-state"); bad.Code != http.StatusConflict {
		t.Fatalf("forged challenge accepted: %d", bad.Code)
	}
	if good := submit(result.Configuration.ID); good.Code != http.StatusOK || !strings.Contains(good.Body.String(), `"ok":true`) {
		t.Fatalf("2FA did not complete: %s", good.Body.String())
	}
	if replay := submit(result.Configuration.ID); replay.Code != http.StatusConflict {
		t.Fatal("completed challenge replayed")
	}
	args, _ := os.ReadFile(log)
	if !strings.Contains(string(args), "--continue --state 2fa_do --result 123456") || !strings.Contains(string(args), "lsd icloud-2fa:") {
		t.Fatal("2FA transition was not completed and probed")
	}
}

func TestConfigurationExpiresAndPreservesDriveChoices(t *testing.T) {
	store := remoteConfigStore{steps: map[string]remoteConfigStep{}}
	step := store.capture("ms", `{"State":"driveid_final","Option":{"Name":"config_driveid","Help":"Select drive","Exclusive":true,"Examples":[{"Value":"drive123","Help":"Documents (business)"}]}}`)
	if step == nil || !step.Question.Exclusive || step.Question.Examples[0].Value != "drive123" {
		t.Fatal("Microsoft drive choice dropped")
	}
	expired := *step
	expired.created = time.Now().Add(-oauthStateTTL - time.Second)
	store.steps["ms"] = expired
	if len(store.all()) != 0 {
		t.Fatal("expired setup advertised")
	}
}

func TestTokenUpdateReturnsDriveSelectionBeforeProbing(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + log + "'\nif [ \"$1\" = listremotes ]; then echo 'ms-drive:'; else echo '{\"State\":\"choose_type_done\",\"Option\":{\"Name\":\"config_type\",\"Help\":\"Choose OneDrive or SharePoint\",\"Exclusive\":true,\"Examples\":[{\"Value\":\"onedrive\",\"Help\":\"OneDrive\"}]}}'; fi\n"
	if err := os.WriteFile(filepath.Join(dir, "rclone"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	req := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(`{"token":"{\"access_token\":\"fixture\"}"}`))
	req.SetPathValue("name", "ms-drive")
	rec := httptest.NewRecorder()
	(&server{}).handleSetRemoteToken(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"configuration"`) {
		t.Fatalf("missing drive setup: %s", rec.Body.String())
	}
	args, _ := os.ReadFile(log)
	if strings.Contains(string(args), "lsd ") {
		t.Fatal("incomplete OneDrive configuration reported as connected")
	}
	pendingRemoteConfigs.Lock()
	delete(pendingRemoteConfigs.steps, "ms-drive")
	pendingRemoteConfigs.Unlock()
}
