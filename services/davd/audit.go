// The access audit trail's refusal half (docs/design/08#2): when davd turns a
// request away it tells onyx-core, so the denial is recorded beside the grant
// that caused it rather than only in this daemon's log. onyx-core owns the trail
// because core is where the grant is recorded and rendered from.
package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	onyxv1 "github.com/innotelinc/onyx/proto/gen/go/onyx/v1"
)

// auditQueueDepth bounds the refusals waiting to be recorded. A refusals queue
// exists to keep the audit off the request path, not to grow without limit, and
// a full queue is itself worth a warning.
const auditQueueDepth = 256

// auditWriteTimeout bounds one audit RPC. A core that has gone away must not
// hold a slot forever.
const auditWriteTimeout = 3 * time.Second

// auditDenials returns the sink the share handlers call on every refusal. It
// connects to onyx-core once and hands requests to a background queue, so a
// refusal neither waits on the audit nor fails because of it: the caller is
// still refused, and a failure to record is logged rather than swallowed.
//
// A core that cannot be dialled degrades to a sink that only logs, which keeps
// a deployment without the audit trail serving shares.
func auditDenials(socketDir string) func(share, user, method, reason string) {
	socket := filepath.Join(socketDir, "onyx-core.sock")
	conn, err := grpc.NewClient("unix://"+socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		slog.Warn("access denials will not be recorded: could not reach onyx-core",
			"socket", socket, "error", err)
		return func(share, user, method, reason string) {
			slog.Warn("access denied (not recorded)", "share", share, "user", user, "method", method, "reason", reason)
		}
	}
	client := onyxv1.NewCoreClient(conn)
	queue := make(chan *onyxv1.RecordAccessDenialRequest, auditQueueDepth)

	go func() {
		for req := range queue {
			ctx, cancel := context.WithTimeout(context.Background(), auditWriteTimeout)
			_, err := client.RecordAccessDenial(ctx, req)
			cancel()
			if err != nil {
				slog.Warn("could not record an access denial",
					"share", req.GetShare(), "user", req.GetUsername(), "error", err)
			}
		}
	}()

	return func(share, user, method, reason string) {
		select {
		case queue <- &onyxv1.RecordAccessDenialRequest{
			Share:    share,
			Username: user,
			Method:   method,
			Reason:   reason,
		}:
		default:
			slog.Warn("the access audit queue is full; a denial was not recorded",
				"share", share, "user", user, "method", method)
		}
	}
}
