// Package auth verifies Supabase access tokens and enforces roles (FR-AU-03/04).
//
// The browser signs in with Supabase Auth and sends the access token as a
// Bearer header. We verify it against Supabase's JWKS (asymmetric signing
// keys, cached and refreshed by keyfunc), then load app.app_user on every
// request, so role changes and deactivation apply on the very next call.
package auth

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"billing-api/internal/db/dbq"
	"billing-api/internal/httpx"
)

const (
	RoleSuperAdmin = "super_admin"
	RoleAdmin      = "admin"
)

// User is the signed-in user, loaded from app.app_user.
type User struct {
	ID        uuid.UUID
	Email     string
	FullName  string
	Role      string
	SessionID *uuid.UUID // Supabase session of this token (login tracking)
	Row       dbq.AppUser
}

func (u User) IsSuperAdmin() bool { return u.Role == RoleSuperAdmin }

type Verifier struct {
	keys   keyfunc.Keyfunc
	issuer string
}

func NewVerifier(ctx context.Context, jwksURL, issuer string) (*Verifier, error) {
	k, err := keyfunc.NewDefaultCtx(ctx, []string{jwksURL})
	if err != nil {
		return nil, err
	}
	return &Verifier{keys: k, issuer: issuer}, nil
}

type claims struct {
	jwt.RegisteredClaims
	SessionID string `json:"session_id"`
}

// Verify checks signature, issuer, audience and expiry.
func (v *Verifier) Verify(token string) (uuid.UUID, *uuid.UUID, error) {
	var c claims
	_, err := jwt.ParseWithClaims(token, &c, v.keys.Keyfunc,
		jwt.WithValidMethods([]string{"ES256", "RS256"}),
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience("authenticated"),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return uuid.Nil, nil, err
	}
	id, err := uuid.Parse(c.Subject)
	if err != nil {
		return uuid.Nil, nil, err
	}
	var sid *uuid.UUID
	if s, err := uuid.Parse(c.SessionID); err == nil {
		sid = &s
	}
	return id, sid, nil
}

type ctxKey int

const (
	userKey ctxKey = iota
	ipKey
)

func FromContext(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(userKey).(User)
	return u, ok
}

// MustUser returns the signed-in user; only valid behind Middleware.
func MustUser(r *http.Request) User {
	u, _ := FromContext(r.Context())
	return u
}

// UserID returns a pointer to the signed-in user's id, or nil.
func UserID(ctx context.Context) *uuid.UUID {
	if u, ok := FromContext(ctx); ok {
		return &u.ID
	}
	return nil
}

// WithUser puts a user in the context (background jobs and tests).
func WithUser(ctx context.Context, u User) context.Context {
	return context.WithValue(ctx, userKey, u)
}

// ClientIP is the caller's address (set by ClientIPMiddleware), for the audit log.
func ClientIP(ctx context.Context) *netip.Addr {
	a, ok := ctx.Value(ipKey).(netip.Addr)
	if !ok {
		return nil
	}
	return &a
}

// ClientIPMiddleware stores r.RemoteAddr (already resolved by chi's RealIP).
func ClientIPMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.RemoteAddr
		if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			host = h
		}
		if a, err := netip.ParseAddr(host); err == nil {
			r = r.WithContext(context.WithValue(r.Context(), ipKey, a.Unmap()))
		}
		next.ServeHTTP(w, r)
	})
}

// Middleware authenticates every request it wraps. Unknown or inactive
// users get 401 (API design 2.2).
func Middleware(v *Verifier, q *dbq.Queries) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || token == "" {
				httpx.WriteProblem(w, r, httpx.Unauthenticated("missing bearer token"))
				return
			}
			id, sid, err := v.Verify(token)
			if err != nil {
				httpx.WriteProblem(w, r, httpx.Unauthenticated("invalid or expired token"))
				return
			}
			row, err := q.GetAppUser(r.Context(), id)
			if errors.Is(err, pgx.ErrNoRows) {
				httpx.WriteProblem(w, r, httpx.Unauthenticated("user is not set up in this application"))
				return
			}
			if err != nil {
				httpx.WriteError(w, r, err)
				return
			}
			if !row.IsActive {
				httpx.WriteProblem(w, r, httpx.Unauthenticated("user is deactivated"))
				return
			}
			u := User{ID: row.ID, Email: row.Email, FullName: row.FullName, Role: string(row.Role), SessionID: sid, Row: row}
			next.ServeHTTP(w, r.WithContext(WithUser(r.Context(), u)))
		})
	}
}

// RequireRole allows only the given roles (the operation's x-roles).
func RequireRole(roles ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u, ok := FromContext(r.Context())
			if !ok {
				httpx.WriteProblem(w, r, httpx.Unauthenticated("not signed in"))
				return
			}
			for _, role := range roles {
				if u.Role == role {
					next.ServeHTTP(w, r)
					return
				}
			}
			httpx.WriteProblem(w, r, httpx.Forbidden("your role cannot perform this action"))
		})
	}
}

// SuperAdmin is RequireRole(super_admin).
var SuperAdmin = RequireRole(RoleSuperAdmin)

// Permissions are the keys the UI uses to show actions (GET /me). The
// server still enforces every one (API design 2.2).
var commonPermissions = []string{
	"dashboard.view", "reports.view", "reports.export",
	"bill.create", "bill.finalize", "bill.copy_to_account", "bill.print", "bill.share", "einvoice.generate",
	"payment.create", "payment.cheque_status",
	"return.create",
	"purchase.create", "purchase_return.create", "stock.transfer", "stock.adjust", "stock.count",
	"catalogue.manage", "item.manage", "party.manage",
	"tally.export",
}

var superAdminPermissions = []string{
	"bill.edit", "bill.cancel", "einvoice.cancel", "credit_limit.override",
	"payment.cancel", "return.approve", "purchase.cancel",
	"item.deactivate", "category.deactivate", "attribute.delete", "party.deactivate",
	"location.manage", "user.manage", "settings.manage", "einvoice.credentials", "tally.import", "audit.view",
}

func Permissions(role string) []string {
	out := append([]string{}, commonPermissions...)
	if role == RoleSuperAdmin {
		out = append(out, superAdminPermissions...)
	}
	return out
}

// MustUserCtx returns the signed-in user from a context.
func MustUserCtx(ctx context.Context) User {
	u, _ := FromContext(ctx)
	return u
}
