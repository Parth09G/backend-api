// Package users handles /me and user management (FR-AU-01/02). Logins
// themselves happen in Supabase Auth; this package keeps app.app_user.
package users

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"billing-api/internal/api"
	"billing-api/internal/app"
	"billing-api/internal/audit"
	"billing-api/internal/auth"
	"billing-api/internal/db/dbq"
	"billing-api/internal/httpx"
	"billing-api/internal/platform/supabase"
)

type handler struct{ a *app.App }

// Routes mounts /users (Super Admin only). /me is mounted by MeRoute.
func Routes(a *app.App) func(chi.Router) {
	h := handler{a}
	return func(r chi.Router) {
		r.Use(auth.SuperAdmin)
		r.Get("/", httpx.H(h.list))
		r.Post("/", httpx.H(h.invite))
		r.Patch("/{id}", httpx.H(h.update))
		r.Post("/{id}/deactivate", httpx.H(h.deactivate))
		r.Post("/{id}/reactivate", httpx.H(h.reactivate))
		r.Post("/{id}/password-reset", httpx.H(h.passwordReset))
	}
}

// Me handles GET /me. A new Supabase session id is recorded as a login in
// the audit log (FR-AL-01) and updates last_login_at.
func Me(a *app.App) http.HandlerFunc {
	return httpx.H(func(w http.ResponseWriter, r *http.Request) error {
		u := auth.MustUser(r)
		row := u.Row
		if u.SessionID != nil && (row.LastSessionID == nil || *row.LastSessionID != *u.SessionID) {
			err := a.Tx(r.Context(), func(_ pgx.Tx, q *dbq.Queries) error {
				if err := q.TouchAppUserLogin(r.Context(), dbq.TouchAppUserLoginParams{ID: u.ID, SessionID: u.SessionID}); err != nil {
					return err
				}
				return audit.Log(r.Context(), q, "login", "app_user", nil, nil, map[string]any{"user_id": u.ID})
			})
			if err != nil {
				slog.WarnContext(r.Context(), "record login", "err", err)
			} else if fresh, err := a.Q.GetAppUser(r.Context(), u.ID); err == nil {
				row = fresh
			}
		}
		httpx.JSON(w, http.StatusOK, api.Me{User: toUser(row), Permissions: auth.Permissions(u.Role)})
		return nil
	})
}

func toUser(u dbq.AppUser) api.User {
	return api.User{
		ID: u.ID, FullName: u.FullName, Email: u.Email, Mobile: u.Mobile, Role: string(u.Role),
		IsActive: u.IsActive, LastLoginAt: u.LastLoginAt, CreatedAt: u.CreatedAt,
	}
}

func (h handler) list(w http.ResponseWriter, r *http.Request) error {
	rows, err := h.a.Q.ListAppUsers(r.Context())
	if err != nil {
		return err
	}
	out := make([]api.User, len(rows))
	for i, u := range rows {
		out[i] = toUser(u)
	}
	httpx.JSON(w, http.StatusOK, out)
	return nil
}

// invite creates the Supabase Auth user (which emails the invite) and then
// the profile. If the profile insert fails the Auth user is deleted again.
func (h handler) invite(w http.ResponseWriter, r *http.Request) error {
	var in api.UserInvite
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	ctx := r.Context()
	id, err := h.a.Auth.Invite(ctx, in.Email, map[string]any{"full_name": in.FullName})
	if errors.Is(err, supabase.ErrEmailExists) {
		return &httpx.Problem{Status: http.StatusConflict, Code: httpx.CodeDuplicate, Detail: "a user with this email already exists",
			Errors: []httpx.FieldError{{Field: "email", Message: "already exists"}}}
	}
	if err != nil {
		return err
	}
	var row dbq.AppUser
	err = h.a.Tx(ctx, func(_ pgx.Tx, q *dbq.Queries) error {
		var err error
		row, err = q.InsertAppUser(ctx, dbq.InsertAppUserParams{
			ID: id, FullName: strings.TrimSpace(in.FullName), Email: in.Email, Mobile: in.Mobile, Role: dbq.UserRole(in.Role),
		})
		if err != nil {
			return err
		}
		return audit.Log(ctx, q, "create", "app_user", nil, nil, toUser(row))
	})
	if err != nil {
		if derr := h.a.Auth.DeleteUser(ctx, id); derr != nil {
			slog.ErrorContext(ctx, "invite rollback: delete auth user", "err", derr, "user_id", id)
		}
		return err
	}
	httpx.JSON(w, http.StatusCreated, toUser(row))
	return nil
}

func (h handler) userID(r *http.Request) (uuid.UUID, error) { return httpx.PathUUID(r, "id") }

func (h handler) update(w http.ResponseWriter, r *http.Request) error {
	id, err := h.userID(r)
	if err != nil {
		return err
	}
	var in api.UserPatch
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	ctx := r.Context()
	var row dbq.AppUser
	err = h.a.Tx(ctx, func(_ pgx.Tx, q *dbq.Queries) error {
		before, err := q.GetAppUser(ctx, id)
		if err != nil {
			return userNotFound(err)
		}
		p := dbq.UpdateAppUserParams{ID: id, FullName: in.FullName}
		if in.Mobile != nil {
			p.SetMobile = true
			if strings.TrimSpace(*in.Mobile) != "" {
				p.Mobile = in.Mobile
			}
		}
		if in.Role != nil {
			role := dbq.UserRole(*in.Role)
			p.Role = &role
		}
		row, err = q.UpdateAppUser(ctx, p)
		if err != nil {
			return err
		}
		return audit.Log(ctx, q, "update", "app_user", nil, toUser(before), toUser(row))
	})
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, toUser(row))
	return nil
}

func (h handler) setActive(w http.ResponseWriter, r *http.Request, active bool) error {
	id, err := h.userID(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	err = h.a.Tx(ctx, func(_ pgx.Tx, q *dbq.Queries) error {
		before, err := q.GetAppUser(ctx, id)
		if err != nil {
			return userNotFound(err)
		}
		row, err := q.SetAppUserActive(ctx, dbq.SetAppUserActiveParams{ID: id, IsActive: active})
		if err != nil {
			return err // LAST_SUPER_ADMIN -> 409
		}
		action := "deactivate"
		if active {
			action = "reactivate"
		}
		return audit.Log(ctx, q, action, "app_user", nil, toUser(before), toUser(row))
	})
	if err != nil {
		return err
	}
	httpx.NoContent(w)
	return nil
}

func (h handler) deactivate(w http.ResponseWriter, r *http.Request) error {
	return h.setActive(w, r, false)
}
func (h handler) reactivate(w http.ResponseWriter, r *http.Request) error {
	return h.setActive(w, r, true)
}

func (h handler) passwordReset(w http.ResponseWriter, r *http.Request) error {
	id, err := h.userID(r)
	if err != nil {
		return err
	}
	u, err := h.a.Q.GetAppUser(r.Context(), id)
	if err != nil {
		return userNotFound(err)
	}
	if err := h.a.Auth.SendPasswordReset(r.Context(), u.Email); err != nil {
		return err
	}
	if err := audit.Log(r.Context(), h.a.Q, "password_reset", "app_user", nil, nil, map[string]any{"user_id": id}); err != nil {
		return err
	}
	httpx.NoContent(w)
	return nil
}

func userNotFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return httpx.NotFound("user")
	}
	return err
}
