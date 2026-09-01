package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"regexp"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	onyxv1 "onyx.dev/onyx/proto/gen/go/onyx/v1"
)

// shareNameRe: share names are stable ids used in paths, exports and SMB share
// sections (docs/design/05#6). Keep them conservative.
var shareNameRe = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

// protoName maps a protocol enum to its DB key; returns ("", false) for
// unknown values so we never persist junk.
func protoName(p onyxv1.ShareProtocol) (string, bool) {
	switch p {
	case onyxv1.ShareProtocol_SHARE_PROTOCOL_SMB:
		return "smb", true
	case onyxv1.ShareProtocol_SHARE_PROTOCOL_NFS:
		return "nfs", true
	default:
		return "", false
	}
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
		switch p {
		case "smb":
			protos = append(protos, onyxv1.ShareProtocol_SHARE_PROTOCOL_SMB)
		case "nfs":
			protos = append(protos, onyxv1.ShareProtocol_SHARE_PROTOCOL_NFS)
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
		return nil, status.Error(codes.InvalidArgument, "at least one protocol must be enabled (smb, nfs)")
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
	// Best-effort: generate the daemon config via onyx-shared. On failure the
	// share still exists (intent recorded); reconciliation will retry later.
	if s.shared != nil {
		if _, err := s.shared.RenderConfig(ctx, &onyxv1.RenderConfigRequest{Share: share}); err != nil {
			slogWarn("render config for share", "share", req.Name, "error", err)
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
