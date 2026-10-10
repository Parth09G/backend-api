// Package audit writes the append-only audit log (FR-AL-01): every change
// to data records who, what, before/after and the client IP, in the same
// transaction as the change.
package audit

import (
	"context"
	"encoding/json"

	"billing-api/internal/auth"
	"billing-api/internal/db/dbq"
)

// Log records one action. before/after are marshalled to JSON (nil = none).
func Log(ctx context.Context, q *dbq.Queries, action, entity string, entityID *int64, before, after any) error {
	return q.InsertAudit(ctx, dbq.InsertAuditParams{
		UserID:     auth.UserID(ctx),
		Action:     action,
		Entity:     entity,
		EntityID:   entityID,
		BeforeData: toJSON(before),
		AfterData:  toJSON(after),
		IpAddress:  auth.ClientIP(ctx),
	})
}

func toJSON(v any) []byte {
	if v == nil {
		return nil
	}
	if b, ok := v.([]byte); ok {
		return b
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// ID is a helper for the entity id argument.
func ID(id int64) *int64 { return &id }
