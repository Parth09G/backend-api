// Package idem makes document-creating POSTs safe to retry (API design 2.4).
//
// The key is reserved in app.api_idempotency inside the same transaction as
// the document. A concurrent duplicate blocks on the primary key until the
// first request commits, then finds the stored response and replays it. If
// the first request fails, its transaction (and the key) is rolled back, so
// the retry runs normally.
package idem

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"billing-api/internal/app"
	"billing-api/internal/auth"
	"billing-api/internal/db/dbq"
	"billing-api/internal/httpx"
)

// Result is what a handler produces; it is stored for replays.
type Result struct {
	Status   int
	Body     any
	ETag     int32  // bill version; 0 = no ETag
	Location string // for 201 Created
}

type stored struct {
	Body     json.RawMessage `json:"body"`
	ETag     int32           `json:"etag,omitempty"`
	Location string          `json:"location,omitempty"`
}

// Fn does the work inside the transaction.
type Fn func(ctx context.Context, tx pgx.Tx, q *dbq.Queries) (Result, error)

// Key reads and validates the Idempotency-Key header.
func Key(r *http.Request) (uuid.UUID, error) {
	k, err := uuid.Parse(r.Header.Get("Idempotency-Key"))
	if err != nil {
		return uuid.Nil, httpx.Validation("Idempotency-Key", "header with a UUID is required for this request")
	}
	return k, nil
}

// Run executes fn at most once for (user, Idempotency-Key) and writes the
// response (or the stored one). body is the raw request body.
func Run(w http.ResponseWriter, r *http.Request, a *app.App, body []byte, fn Fn) error {
	key, err := Key(r)
	if err != nil {
		return err
	}
	user := auth.MustUser(r)
	sum := sha256.Sum256(body)
	ctx := r.Context()

	var (
		replay *dbq.ApiIdempotency
		res    Result
		saved  []byte
	)
	err = a.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		replay = nil
		_, err := q.ReserveIdempotencyKey(ctx, dbq.ReserveIdempotencyKeyParams{
			UserID: user.ID, Key: key, Method: r.Method, Path: r.URL.Path, RequestHash: sum[:],
		})
		if errors.Is(err, pgx.ErrNoRows) {
			row, err := q.GetIdempotencyKey(ctx, dbq.GetIdempotencyKeyParams{UserID: user.ID, Key: key})
			if err != nil {
				return err
			}
			replay = &row
			return nil
		}
		if err != nil {
			return err
		}
		res, err = fn(ctx, tx, q)
		if err != nil {
			return err
		}
		b, err := json.Marshal(res.Body)
		if err != nil {
			return err
		}
		saved, err = json.Marshal(stored{Body: b, ETag: res.ETag, Location: res.Location})
		if err != nil {
			return err
		}
		return q.StoreIdempotentResponse(ctx, dbq.StoreIdempotentResponseParams{
			StatusCode: ptr(int32(res.Status)), Response: saved, UserID: user.ID, Key: key,
		})
	})
	if err != nil {
		return err
	}

	if replay != nil {
		if replay.Method != r.Method || replay.Path != r.URL.Path || !bytes.Equal(replay.RequestHash, sum[:]) {
			return httpx.Conflict(httpx.CodeIdempotencyReuse, "this Idempotency-Key was used for a different request")
		}
		if replay.StatusCode == nil {
			return httpx.Conflict(httpx.CodeConflict, "the first request with this Idempotency-Key is still running")
		}
		var s stored
		if err := json.Unmarshal(replay.Response, &s); err != nil {
			return err
		}
		w.Header().Set("Idempotent-Replay", "true")
		writeStored(w, int(*replay.StatusCode), s)
		return nil
	}
	var s stored
	_ = json.Unmarshal(saved, &s)
	writeStored(w, res.Status, s)
	return nil
}

func writeStored(w http.ResponseWriter, status int, s stored) {
	if s.ETag > 0 {
		w.Header().Set("ETag", fmt.Sprintf(`"%d"`, s.ETag))
	}
	if s.Location != "" {
		w.Header().Set("Location", s.Location)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(s.Body)
	_, _ = w.Write([]byte("\n"))
}

func ptr[T any](v T) *T { return &v }
