package main

import (
	"context"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	onyxv1 "github.com/innotelinc/onyx/proto/gen/go/onyx/v1"
)

// The access audit trail (docs/design/08#2). A grant decides who may reach a
// share, so both halves of it are recorded here: the change itself (who set
// what, and by whose hand) and the refusals it caused. It lives in onyx-core
// because core is where the grant is recorded and where the share backends are
// rendered from — one place to answer "why can this person not reach this
// share", instead of correlating a console page with a daemon's log.

// Access event kinds, as they appear in AccessEvent.kind.
const (
	eventGrant  = "grant"
	eventRevoke = "revoke"
	eventDenied = "denied"
)

// defaultAccessEventLimit is what a caller gets without asking for a size, and
// maxAccessEventLimit caps what it can ask for: the trail is useful in pages,
// not as an export.
const (
	defaultAccessEventLimit = 100
	maxAccessEventLimit     = 500
)

// recordAccessEvent appends one line. It is deliberately best-effort at the call
// sites that serve a request: a full disk must not turn a granted share into a
// failed one, but it is always reported to the caller who caused it.
func (s *server) recordAccessEvent(ctx context.Context, kind, share, username, mode, actor, detail string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO access_events (kind, share, username, mode, actor, detail) VALUES (?, ?, ?, ?, ?, ?)`,
		kind, share, username, mode, accessEventActor(actor), detail,
	)
	return err
}

// RecordAccessDenial appends a refusal. onyx-davd calls it when it turns a
// request away so the denial is recorded beside the grant that caused it.
func (s *server) RecordAccessDenial(ctx context.Context, req *onyxv1.RecordAccessDenialRequest) (*onyxv1.RecordAccessDenialResponse, error) {
	if req.GetShare() == "" {
		return nil, status.Error(codes.InvalidArgument, "a denial needs the share it was refused for")
	}
	if err := s.recordAccessEvent(ctx, eventDenied, req.GetShare(), req.GetUsername(), "", req.GetUsername(), strings.TrimSpace(req.GetMethod()+" "+req.GetReason())); err != nil {
		return nil, status.Errorf(codes.Internal, "record denial: %v", err)
	}
	return &onyxv1.RecordAccessDenialResponse{}, nil
}

// ListAccessEvents returns the trail, newest first.
func (s *server) ListAccessEvents(ctx context.Context, req *onyxv1.ListAccessEventsRequest) (*onyxv1.ListAccessEventsResponse, error) {
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = defaultAccessEventLimit
	}
	if limit > maxAccessEventLimit {
		limit = maxAccessEventLimit
	}

	query := `SELECT id, ts, kind, share, username, mode, actor, detail FROM access_events`
	var (
		where []string
		args  []any
	)
	if share := strings.TrimSpace(req.GetShare()); share != "" {
		where = append(where, "share = ?")
		args = append(args, share)
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "query access events: %v", err)
	}
	defer rows.Close()

	resp := &onyxv1.ListAccessEventsResponse{}
	for rows.Next() {
		var e onyxv1.AccessEvent
		if err := rows.Scan(&e.Id, &e.Ts, &e.Kind, &e.Share, &e.Username, &e.Mode, &e.Actor, &e.Detail); err != nil {
			return nil, status.Errorf(codes.Internal, "scan access event: %v", err)
		}
		resp.Events = append(resp.Events, &e)
	}
	return resp, rows.Err()
}

// accessEventActor is who a change is attributed to. The gateway knows the
// operator's identity, but an operator can also be a script through the CLI, so
// an empty actor is named rather than left blank in the trail.
func accessEventActor(actor string) string {
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return "console"
	}
	return actor
}

// accessEventKind classifies a change: setting or changing a grant is a "grant",
// clearing one is a "revoke".
func accessEventKind(mode string) string {
	if mode == "" {
		return eventRevoke
	}
	return eventGrant
}
