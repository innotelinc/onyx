package main

import (
	"context"
	"sort"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	onyxv1 "github.com/innotelinc/onyx/proto/gen/go/onyx/v1"
)

// Samba accounts for Onyx users (docs/design/05#6 SMB row). A granted share
// renders the grantee's name into smb.conf's `valid users`, and Samba only
// admits a name that exists in its own passdb — an account it has never heard of
// is treated as nobody, so the grant would silently do nothing. This is the
// missing half: the account is created, re-passwords, disabled or removed
// through onyx-privd (the only process allowed to touch the passdb), and the
// password never travels on a command line.

// minSambaPassword is the shortest password Onyx will hand to Samba. smbpasswd
// itself accepts anything; a one-character share password is not a credential.
const minSambaPassword = 8

// ProvisionSambaUser creates, re-passwords, disables or removes the Samba
// account behind an Onyx user.
func (s *server) ProvisionSambaUser(ctx context.Context, req *onyxv1.ProvisionSambaUserRequest) (*onyxv1.ProvisionSambaUserResponse, error) {
	action := strings.ToLower(strings.TrimSpace(req.GetAction()))
	if action == "list" {
		// "list" answers what exists; the console needs that on its own, without
		// having to change an account to find out.
		accounts, err := s.sambaAccounts(ctx)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		return &onyxv1.ProvisionSambaUserResponse{Accounts: accounts}, nil
	}
	username := strings.TrimSpace(req.GetUsername())
	if !validUsernameRe.MatchString(username) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid username %q", username)
	}
	switch action {
	case "add", "remove", "disable":
	default:
		return nil, status.Errorf(codes.InvalidArgument, "action must be add, remove, disable or list, got %q", req.GetAction())
	}
	if action == "add" {
		if err := validateSambaPassword(req.GetPassword()); err != nil {
			return nil, err
		}
	}
	if s.privd == nil {
		return nil, status.Error(codes.Unavailable, "onyx-privd is not reachable, so the Samba account cannot be managed")
	}

	privReq := &onyxv1.PrivRequest{
		Op:   onyxv1.PrivOp_SAMBA_USER,
		Args: []string{action, username},
	}
	if action == "add" {
		// The password rides the request's secret field, which privd writes to
		// smbpasswd's stdin: argv is world-readable in /proc.
		privReq.Secret = []byte(req.GetPassword())
	}
	resp, err := s.privd.Run(ctx, privReq)
	if err := privdOK(resp, err, "samba account "+action); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	accounts, listErr := s.sambaAccounts(ctx)
	if listErr != nil && action != "remove" {
		// The change itself worked; the console just cannot say what the full
		// list looks like now. Report the change rather than failing it.
		return &onyxv1.ProvisionSambaUserResponse{Provisioned: true}, nil
	}
	// "provisioned" means the account can sign in after this call, so only an
	// add that succeeded sets it: a disabled account still exists in the passdb
	// but admits nobody.
	return &onyxv1.ProvisionSambaUserResponse{Provisioned: action == "add", Accounts: accounts}, nil
}

// validateSambaPassword refuses the passwords that would produce an account
// that looks provisioned but cannot be used, or that smbpasswd would misread.
func validateSambaPassword(password string) error {
	switch {
	case password == "":
		return status.Error(codes.InvalidArgument, "a password is required to create the account")
	case len(password) < minSambaPassword:
		return status.Errorf(codes.InvalidArgument, "password must be at least %d characters", minSambaPassword)
	case strings.ContainsAny(password, "\n\r"):
		// smbpasswd reads the password and its confirmation as two lines, so a
		// newline would truncate it (or split it into the confirmation).
		return status.Error(codes.InvalidArgument, "password must not contain a line break")
	default:
		return nil
	}
}

// sambaAccounts lists the accounts Samba knows, so the console can show which
// Onyx users can actually reach an SMB share.
func (s *server) sambaAccounts(ctx context.Context) ([]string, error) {
	if s.privd == nil {
		return nil, status.Error(codes.Unavailable, "onyx-privd is not reachable")
	}
	resp, err := s.privd.Run(ctx, &onyxv1.PrivRequest{
		Op:   onyxv1.PrivOp_SAMBA_USER,
		Args: []string{"list"},
	})
	if err := privdOK(resp, err, "list samba accounts"); err != nil {
		return nil, err
	}
	// `pdbedit -L` prints one `username:uid:full name` line per account. Only
	// the name is used, and an unparseable line is skipped rather than guessed.
	var names []string
	for _, line := range strings.Split(string(resp.Stdout), "\n") {
		name, _, _ := strings.Cut(strings.TrimSpace(line), ":")
		if name != "" && validUsernameRe.MatchString(name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}
