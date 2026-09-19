package main

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	onyxv1 "github.com/innotelinc/onyx/proto/gen/go/onyx/v1"
)

// sambaServer builds a core server whose privd is the recording fake.
func sambaServer(t *testing.T) (*server, *fakePrivd) {
	t.Helper()
	db, err := openDB(t.TempDir())
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	privd := &fakePrivd{pdbedit: "dana:1000:Dana\n"}
	return &server{db: db, privd: privd}, privd
}

func codeOf(t *testing.T, err error) codes.Code {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("not a status error: %v", err)
	}
	return st.Code()
}

// The password is a credential, so it must never be parsed from argv by
// smbpasswd and never chosen by us: what core refuses, privd never sees. This
// asserts both halves — the error and that nothing was run.
func TestProvisionSambaUserRefusesWeakPasswords(t *testing.T) {
	cases := []struct {
		name     string
		password string
	}{
		{"empty", ""},
		{"too short", "short"},
		{"line break", "longenough\nsecond"},
		{"carriage return", "longenough\rsecond"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, privd := sambaServer(t)
			_, err := s.ProvisionSambaUser(context.Background(), &onyxv1.ProvisionSambaUserRequest{
				Action:   "add",
				Username: "dana",
				Password: tc.password,
			})
			if got := codeOf(t, err); got != codes.InvalidArgument {
				t.Fatalf("code = %v, want InvalidArgument", got)
			}
			if len(privd.samba) != 0 {
				t.Errorf("privd was asked to touch the passdb anyway: %v", privd.samba)
			}
		})
	}
}

// The password must ride the request's secret field (which privd feeds to
// smbpasswd's stdin), not the argument list, where /proc exposes it.
func TestProvisionSambaUserPasswordTravelsOffArgv(t *testing.T) {
	s, privd := sambaServer(t)
	if _, err := s.ProvisionSambaUser(context.Background(), &onyxv1.ProvisionSambaUserRequest{
		Action:   "add",
		Username: "dana",
		Password: "correct horse",
	}); err != nil {
		t.Fatalf("ProvisionSambaUser: %v", err)
	}
	if len(privd.samba) == 0 {
		t.Fatal("privd never saw the request")
	}
	if got := privd.samba[0]; got != "add dana secret:correct horse" {
		t.Fatalf("call = %q", got)
	}
	// The change is followed by a listing so the console can show the result;
	// that listing carries no secret.
	if last := privd.samba[len(privd.samba)-1]; last != "list secret:" {
		t.Errorf("last call = %q, want a plain listing", last)
	}
}

// A provisioned account reads its password back full, and the name reaches the
// `valid users` list core renders.
func TestProvisionSambaUserReportsTheAccounts(t *testing.T) {
	s, privd := sambaServer(t)
	privd.pdbedit = "dana:1000:Dana\nroot:0:root\nbad name:1:x\n"

	resp, err := s.ProvisionSambaUser(context.Background(), &onyxv1.ProvisionSambaUserRequest{
		Action:   "add",
		Username: "dana",
		Password: "correct horse",
	})
	if err != nil {
		t.Fatalf("ProvisionSambaUser: %v", err)
	}
	if !resp.GetProvisioned() {
		t.Error("a created account must report provisioned")
	}
	// Unparseable names are skipped, never guessed into the allow list.
	if got := resp.GetAccounts(); len(got) != 2 || got[0] != "dana" || got[1] != "root" {
		t.Errorf("accounts = %v", got)
	}
}

// Disable and remove report provisioned=false: the account must not stay in a
// rendered `valid users` list after being taken away.
func TestProvisionSambaUserRemoveAndDisable(t *testing.T) {
	for _, action := range []string{"disable", "remove"} {
		t.Run(action, func(t *testing.T) {
			s, privd := sambaServer(t)
			resp, err := s.ProvisionSambaUser(context.Background(), &onyxv1.ProvisionSambaUserRequest{
				Action:   action,
				Username: "dana",
			})
			if err != nil {
				t.Fatalf("ProvisionSambaUser: %v", err)
			}
			if resp.GetProvisioned() {
				t.Errorf("%s must not report a provisioned account", action)
			}
			if len(privd.samba) == 0 || privd.samba[0] != action+" dana secret:" {
				t.Errorf("calls = %v", privd.samba)
			}
		})
	}
}

func TestProvisionSambaUserRejectsBadInput(t *testing.T) {
	s, privd := sambaServer(t)
	cases := []struct {
		name string
		req  *onyxv1.ProvisionSambaUserRequest
	}{
		{"unknown action", &onyxv1.ProvisionSambaUserRequest{Action: "promote", Username: "dana", Password: "correct horse"}},
		{"missing change", &onyxv1.ProvisionSambaUserRequest{Username: "dana", Password: "correct horse"}},
		{"bad username", &onyxv1.ProvisionSambaUserRequest{Action: "add", Username: "../etc/passwd", Password: "correct horse"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.ProvisionSambaUser(context.Background(), tc.req)
			if got := codeOf(t, err); got != codes.InvalidArgument {
				t.Fatalf("code = %v, want InvalidArgument", got)
			}
		})
	}
	if len(privd.samba) != 0 {
		t.Errorf("privd was called for an invalid request: %v", privd.samba)
	}
}

// "list" answers what exists without changing anything, so a console can show
// which Onyx users can actually reach an SMB share.
func TestProvisionSambaUserListDoesNotChangeAnything(t *testing.T) {
	s, privd := sambaServer(t)
	privd.pdbedit = "dana:1000:Dana\n"

	resp, err := s.ProvisionSambaUser(context.Background(), &onyxv1.ProvisionSambaUserRequest{Action: "list"})
	if err != nil {
		t.Fatalf("ProvisionSambaUser: %v", err)
	}
	if resp.GetProvisioned() {
		t.Error("a listing provisions nothing")
	}
	if got := resp.GetAccounts(); len(got) != 1 || got[0] != "dana" {
		t.Errorf("accounts = %v", got)
	}
	if len(privd.samba) != 1 || privd.samba[0] != "list secret:" {
		t.Errorf("calls = %v", privd.samba)
	}
}

// Without privd there is no way to touch the passdb, and saying so beats
// reporting a grant that Samba will treat as nobody.
func TestProvisionSambaUserWithoutPrivd(t *testing.T) {
	db, err := openDB(t.TempDir())
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()
	s := &server{db: db}

	_, err = s.ProvisionSambaUser(context.Background(), &onyxv1.ProvisionSambaUserRequest{
		Action:   "add",
		Username: "dana",
		Password: "correct horse",
	})
	if got := codeOf(t, err); got != codes.Unavailable {
		t.Fatalf("code = %v, want Unavailable", got)
	}
	if _, err := s.ProvisionSambaUser(context.Background(), &onyxv1.ProvisionSambaUserRequest{Action: "list"}); err == nil {
		t.Error("listing without privd must fail rather than report no accounts")
	}
}

// A failed smbpasswd must not be reported as a provisioned account.
func TestProvisionSambaUserPropagatesFailure(t *testing.T) {
	s, privd := sambaServer(t)
	privd.failOp = onyxv1.PrivOp_SAMBA_USER

	_, err := s.ProvisionSambaUser(context.Background(), &onyxv1.ProvisionSambaUserRequest{
		Action:   "add",
		Username: "dana",
		Password: "correct horse",
	})
	if got := codeOf(t, err); got != codes.Internal {
		t.Fatalf("code = %v, want Internal", got)
	}
}
