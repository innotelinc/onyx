package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"regexp"
	"sort"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	onyxv1 "github.com/innotelinc/onyx/proto/gen/go/onyx/v1"
)

// shareNameRe: share names are stable ids used in paths, exports and SMB share
// sections (docs/design/05#6). Keep them conservative.
var shareNameRe = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

// validUsernameRe keeps a grantee name renderable into every share backend: a
// name with whitespace or a quote in it could break out of a `valid users`
// line or a davd.conf list, so it is refused at the door instead.
var validUsernameRe = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,64}$`)

// protoName maps a protocol enum to its DB key; returns ("", false) for
// unknown values so we never persist junk.
func protoName(p onyxv1.ShareProtocol) (string, bool) {
	switch p {
	case onyxv1.ShareProtocol_SHARE_PROTOCOL_SMB:
		return "smb", true
	case onyxv1.ShareProtocol_SHARE_PROTOCOL_NFS:
		return "nfs", true
	case onyxv1.ShareProtocol_SHARE_PROTOCOL_FTP:
		return "ftp", true
	case onyxv1.ShareProtocol_SHARE_PROTOCOL_SFTP:
		return "sftp", true
	case onyxv1.ShareProtocol_SHARE_PROTOCOL_WEBDAV:
		return "webdav", true
	case onyxv1.ShareProtocol_SHARE_PROTOCOL_RSYNC:
		return "rsync", true
	default:
		return "", false
	}
}

// protoFromName is the inverse of protoName: the DB key back to the enum, so
// the persisted protocol list round-trips for every protocol (docs/design/05#6).
func protoFromName(name string) (onyxv1.ShareProtocol, bool) {
	switch name {
	case "smb":
		return onyxv1.ShareProtocol_SHARE_PROTOCOL_SMB, true
	case "nfs":
		return onyxv1.ShareProtocol_SHARE_PROTOCOL_NFS, true
	case "ftp":
		return onyxv1.ShareProtocol_SHARE_PROTOCOL_FTP, true
	case "sftp":
		return onyxv1.ShareProtocol_SHARE_PROTOCOL_SFTP, true
	case "webdav":
		return onyxv1.ShareProtocol_SHARE_PROTOCOL_WEBDAV, true
	case "rsync":
		return onyxv1.ShareProtocol_SHARE_PROTOCOL_RSYNC, true
	default:
		return onyxv1.ShareProtocol_SHARE_PROTOCOL_UNSPECIFIED, false
	}
}

// Access modes a grant may carry (docs/design/08#2). Anything else is refused
// rather than stored, so the renderers only ever see these two words.
const (
	accessRead      = "read"
	accessReadWrite = "read-write"
)

func validAccessMode(mode string) bool {
	return mode == accessRead || mode == accessReadWrite
}

// loadShareAccess reads the grants for one share (empty name = every share).
// Rows come back in a deterministic order so renders are stable.
func (s *server) loadShareAccess(share, username string) (map[string][]*onyxv1.ShareAccess, error) {
	query := `SELECT share, username, mode FROM share_access`
	var (
		where []string
		args  []any
	)
	if share != "" {
		where = append(where, "share = ?")
		args = append(args, share)
	}
	if username != "" {
		where = append(where, "username = ?")
		args = append(args, username)
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY share, username"

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byShare := map[string][]*onyxv1.ShareAccess{}
	for rows.Next() {
		var a onyxv1.ShareAccess
		if err := rows.Scan(&a.Share, &a.Username, &a.Mode); err != nil {
			return nil, err
		}
		byShare[a.Share] = append(byShare[a.Share], &a)
	}
	return byShare, rows.Err()
}

// attachShareAccess fills each share's access list, so every path that reports
// or renders a share (ListShares, GetShare, the config applier) sees the grants
// the Users page wrote.
func (s *server) attachShareAccess(shares []*onyxv1.Share) error {
	if len(shares) == 0 {
		return nil
	}
	byShare, err := s.loadShareAccess("", "")
	if err != nil {
		return err
	}
	for _, share := range shares {
		share.Access = byShare[share.Name]
	}
	return nil
}

// SetShareAccess records, changes or removes one grant (docs/design/08#2).
// Removing is what an empty mode means: the row goes away rather than being
// kept with a null, so "no grant" is one state and not two.
func (s *server) SetShareAccess(ctx context.Context, req *onyxv1.SetShareAccessRequest) (*onyxv1.SetShareAccessResponse, error) {
	a := req.GetAccess()
	if a == nil || a.Share == "" || a.Username == "" {
		return nil, status.Error(codes.InvalidArgument, "access requires a share and a username")
	}
	if !shareNameRe.MatchString(a.Share) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid share name %q", a.Share)
	}
	if !validUsernameRe.MatchString(a.Username) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid username %q", a.Username)
	}
	if a.Mode != "" && !validAccessMode(a.Mode) {
		return nil, status.Errorf(codes.InvalidArgument, "mode must be %q, %q or empty to remove the grant, got %q", accessRead, accessReadWrite, a.Mode)
	}
	// The share has to exist: a grant on a share nobody can mount is a promise
	// the backends cannot keep.
	if _, err := s.getShare(ctx, a.Share); err != nil {
		return nil, err
	}

	if a.Mode == "" {
		if _, err := s.db.Exec(`DELETE FROM share_access WHERE share = ? AND username = ?`, a.Share, a.Username); err != nil {
			return nil, status.Errorf(codes.Internal, "remove share access: %v", err)
		}
	} else {
		if _, err := s.db.Exec(
			`INSERT INTO share_access (share, username, mode) VALUES (?, ?, ?)
			 ON CONFLICT(share, username) DO UPDATE SET mode = excluded.mode`,
			a.Share, a.Username, a.Mode,
		); err != nil {
			return nil, status.Errorf(codes.Internal, "record share access: %v", err)
		}
	}

	// The grant is only real once the daemons serve it, so re-render now
	// (docs/design/02#6 steps 3-4). A failure leaves the row recorded and the
	// reconciler's next apply retries, exactly like a share mutation.
	if s.config != nil {
		if err := s.config.apply(ctx); err != nil {
			slogWarn("apply daemon config after access change", "share", a.Share, "user", a.Username, "error", err)
		}
	}
	return &onyxv1.SetShareAccessResponse{Access: a}, nil
}

// ListShareAccess reports grants, optionally filtered by share and/or username.
func (s *server) ListShareAccess(ctx context.Context, req *onyxv1.ListShareAccessRequest) (*onyxv1.ListShareAccessResponse, error) {
	byShare, err := s.loadShareAccess(req.GetShare(), req.GetUsername())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "query share access: %v", err)
	}
	resp := &onyxv1.ListShareAccessResponse{}
	for _, share := range sortedShareKeys(byShare) {
		resp.Access = append(resp.Access, byShare[share]...)
	}
	return resp, nil
}

// sortedShareKeys keeps ListShareAccess deterministic (the query is ordered by
// share already, but the map is not).
func sortedShareKeys(m map[string][]*onyxv1.ShareAccess) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// scanShare converts a DB row into the proto Share.
func scanShare(row interface{ Scan(...any) error }) (*onyxv1.Share, error) {
	var name, path, comment, protocols string
	var readonly int
	if err := row.Scan(&name, &path, &comment, &readonly, &protocols); err != nil {
		return nil, err
	}
	var protos []onyxv1.ShareProtocol
	for _, p := range strings.Split(protocols, ",") {
		if proto, ok := protoFromName(p); ok {
			protos = append(protos, proto)
		}
	}
	return &onyxv1.Share{
		Name:      name,
		Path:      path,
		Comment:   comment,
		Readonly:  readonly != 0,
		Protocols: protos,
	}, nil
}

func (s *server) CreateShare(ctx context.Context, req *onyxv1.CreateShareRequest) (*onyxv1.Share, error) {
	if !shareNameRe.MatchString(req.Name) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid share name %q (must match %s)", req.Name, shareNameRe)
	}
	if req.Path == "" || !strings.HasPrefix(req.Path, "/") {
		return nil, status.Error(codes.InvalidArgument, "share path must be an absolute directory path")
	}
	if len(req.Protocols) == 0 {
		return nil, status.Error(codes.InvalidArgument, "at least one protocol must be enabled (smb, nfs, ftp, sftp, webdav, rsync)")
	}

	var keys []string
	for _, p := range req.Protocols {
		k, ok := protoName(p)
		if !ok {
			return nil, status.Errorf(codes.InvalidArgument, "unsupported protocol %d", p)
		}
		keys = append(keys, k)
	}

	_, err := s.db.Exec(
		`INSERT INTO shares (name, path, comment, readonly, protocols) VALUES (?, ?, ?, ?, ?)`,
		req.Name, req.Path, req.Comment, b2i(req.Readonly), strings.Join(keys, ","),
	)
	if err != nil {
		if isUniqueErr(err) {
			return nil, status.Errorf(codes.AlreadyExists, "share %q already exists", req.Name)
		}
		return nil, status.Errorf(codes.Internal, "insert share: %v", err)
	}

	share, err := s.getShare(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	// Sync the daemon config: render -> write -> reload through privd
	// (docs/design/02#6 steps 3-4). On failure the share still exists (intent
	// recorded); the reconciler's periodic apply retries.
	if s.config != nil {
		if err := s.config.apply(ctx); err != nil {
			slogWarn("apply daemon config after create", "share", req.Name, "error", err)
		}
	}
	return share, nil
}

func (s *server) ListShares(ctx context.Context, _ *onyxv1.ListSharesRequest) (*onyxv1.ListSharesResponse, error) {
	rows, err := s.db.Query(`SELECT name, path, comment, readonly, protocols FROM shares ORDER BY name`)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "query shares: %v", err)
	}
	defer rows.Close()

	var shares []*onyxv1.Share
	for rows.Next() {
		share, err := scanShare(rows)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "scan share: %v", err)
		}
		shares = append(shares, share)
	}
	if err := s.attachShareAccess(shares); err != nil {
		return nil, status.Errorf(codes.Internal, "load share access: %v", err)
	}
	return &onyxv1.ListSharesResponse{Shares: shares}, nil
}

func (s *server) GetShare(ctx context.Context, req *onyxv1.GetShareRequest) (*onyxv1.Share, error) {
	return s.getShare(ctx, req.Name)
}

func (s *server) getShare(ctx context.Context, name string) (*onyxv1.Share, error) {
	row := s.db.QueryRow(`SELECT name, path, comment, readonly, protocols FROM shares WHERE name = ?`, name)
	share, err := scanShare(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, status.Errorf(codes.NotFound, "share %q does not exist", name)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "query share: %v", err)
	}
	if err := s.attachShareAccess([]*onyxv1.Share{share}); err != nil {
		return nil, status.Errorf(codes.Internal, "load share access: %v", err)
	}
	return share, nil
}

func (s *server) DeleteShare(ctx context.Context, req *onyxv1.DeleteShareRequest) (*onyxv1.DeleteShareResponse, error) {
	res, err := s.db.Exec(`DELETE FROM shares WHERE name = ?`, req.Name)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "delete share: %v", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, status.Errorf(codes.NotFound, "share %q does not exist", req.Name)
	}
	// The grants go with the share: a row naming a share that no longer exists
	// would re-appear as an access rule if the name were ever reused.
	if _, err := s.db.Exec(`DELETE FROM share_access WHERE share = ?`, req.Name); err != nil {
		return nil, status.Errorf(codes.Internal, "delete share access: %v", err)
	}
	// The share is gone: rewrite smb.conf/exports and reload so the removed
	// share stops being served (change-guarded, so no-ops if unrelated).
	if s.config != nil {
		if err := s.config.apply(ctx); err != nil {
			slogWarn("apply daemon config after delete", "share", req.Name, "error", err)
		}
	}
	return &onyxv1.DeleteShareResponse{}, nil
}

// --- helpers ---

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func isUniqueErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func slogWarn(msg string, args ...any) {
	slog.Warn(msg, args...)
}
