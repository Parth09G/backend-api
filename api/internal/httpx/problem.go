// Package httpx holds the HTTP plumbing shared by every endpoint: RFC 9457
// problem errors, the error-returning handler adapter, JSON decoding with
// validation, pagination and query parsing.
package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Stable error codes (openapi.yaml components.schemas.Problem.code).
const (
	CodeValidation       = "VALIDATION_FAILED"
	CodeUnauthenticated  = "UNAUTHENTICATED"
	CodeForbidden        = "FORBIDDEN"
	CodeNotFound         = "NOT_FOUND"
	CodeConflict         = "CONFLICT"
	CodeVersionMismatch  = "VERSION_MISMATCH"
	CodeDuplicate        = "DUPLICATE"
	CodeInsufficient     = "INSUFFICIENT_STOCK"
	CodeCreditLimit      = "CREDIT_LIMIT_EXCEEDED"
	CodeEditNotAllowed   = "EDIT_NOT_ALLOWED"
	CodeEInvoiceWindow   = "EINVOICE_WINDOW_CLOSED"
	CodeInvalidState     = "INVALID_STATE"
	CodeIdempotencyReuse = "IDEMPOTENCY_REPLAY_MISMATCH"
	CodeRateLimited      = "RATE_LIMITED"
	CodeInternal         = "INTERNAL"
)

var titles = map[string]string{
	CodeValidation:       "Some fields are invalid",
	CodeUnauthenticated:  "Sign-in required",
	CodeForbidden:        "Not allowed for your role",
	CodeNotFound:         "Not found",
	CodeConflict:         "Conflict",
	CodeVersionMismatch:  "Changed by someone else; reload and try again",
	CodeDuplicate:        "Already exists",
	CodeInsufficient:     "Not enough stock",
	CodeCreditLimit:      "Credit limit exceeded",
	CodeEditNotAllowed:   "Editing is not allowed",
	CodeEInvoiceWindow:   "E-invoice can no longer be changed (24-hour window closed)",
	CodeInvalidState:     "Not allowed in the current state",
	CodeIdempotencyReuse: "Idempotency-Key reused with a different request",
	CodeRateLimited:      "Too many requests",
	CodeInternal:         "Something went wrong",
}

type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// Problem is an API error rendered as application/problem+json.
type Problem struct {
	Status int
	Code   string
	Detail string
	Errors []FieldError
	// Extra members merged into the body (e.g. short_lines, credit_limit).
	Extra map[string]any
}

func (p *Problem) Error() string {
	if p.Detail != "" {
		return p.Code + ": " + p.Detail
	}
	return p.Code
}

func NewProblem(status int, code, detail string) *Problem {
	return &Problem{Status: status, Code: code, Detail: detail}
}

func Validation(field, msg string) *Problem {
	return &Problem{Status: http.StatusBadRequest, Code: CodeValidation, Errors: []FieldError{{Field: field, Message: msg}}}
}

func ValidationErrors(errs []FieldError) *Problem {
	return &Problem{Status: http.StatusBadRequest, Code: CodeValidation, Errors: errs}
}

func NotFound(what string) *Problem {
	return &Problem{Status: http.StatusNotFound, Code: CodeNotFound, Detail: what + " not found"}
}

func Forbidden(detail string) *Problem {
	return &Problem{Status: http.StatusForbidden, Code: CodeForbidden, Detail: detail}
}

func InvalidState(detail string) *Problem {
	return &Problem{Status: http.StatusConflict, Code: CodeInvalidState, Detail: detail}
}

func Conflict(code, detail string) *Problem {
	return &Problem{Status: http.StatusConflict, Code: code, Detail: detail}
}

func Unauthenticated(detail string) *Problem {
	return &Problem{Status: http.StatusUnauthorized, Code: CodeUnauthenticated, Detail: detail}
}

// WriteProblem renders p with the request id.
func WriteProblem(w http.ResponseWriter, r *http.Request, p *Problem) {
	title := titles[p.Code]
	if title == "" {
		title = http.StatusText(p.Status)
	}
	body := map[string]any{
		"type":   "urn:billing:error:" + strings.ReplaceAll(strings.ToLower(p.Code), "_", "-"),
		"title":  title,
		"status": p.Status,
		"code":   p.Code,
	}
	if p.Detail != "" {
		body["detail"] = p.Detail
	}
	if len(p.Errors) > 0 {
		body["errors"] = p.Errors
	}
	if id := middleware.GetReqID(r.Context()); id != "" {
		body["request_id"] = id
	}
	for k, v := range p.Extra {
		body[k] = v
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(body)
}

// HandlerFunc is an http handler that returns an error instead of writing it.
type HandlerFunc func(w http.ResponseWriter, r *http.Request) error

// H adapts a HandlerFunc; any returned error becomes a problem response.
func H(h HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			WriteError(w, r, err)
		}
	}
}

// WriteError maps any error to a problem response.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	WriteProblem(w, r, ToProblem(r.Context(), err))
}

// friendly names for unique / foreign-key constraints (409 DUPLICATE field).
var constraintFields = map[string]string{
	"purchase_supplier_id_supplier_invoice_no_key": "supplier_invoice_no",
	"party_gstin_uq":                          "gstin",
	"unit_name_key":                           "name",
	"unit_symbol_key":                         "symbol",
	"category_name_key":                       "name",
	"item_category_id_name_key":               "name",
	"item_sku_key":                            "sku",
	"location_name_key":                       "name",
	"app_user_email_key":                      "email",
	"app_user_mobile_key":                     "mobile",
	"category_attribute_category_id_code_key": "code",
	"one_default_billing_location":            "is_default_billing",
}

// ToProblem converts errors from handlers and the database into problems.
func ToProblem(ctx context.Context, err error) *Problem {
	var p *Problem
	if errors.As(err, &p) {
		return p
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return NotFound("record")
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if p := fromPg(pgErr); p != nil {
			return p
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &Problem{Status: http.StatusServiceUnavailable, Code: CodeInternal, Detail: "the request took too long; please retry"}
	}
	if errors.Is(err, context.Canceled) {
		return &Problem{Status: 499, Code: CodeInternal, Detail: "request cancelled"}
	}
	slog.ErrorContext(ctx, "unhandled error", "err", err, "request_id", middleware.GetReqID(ctx))
	return &Problem{Status: http.StatusInternalServerError, Code: CodeInternal}
}

func fromPg(e *pgconn.PgError) *Problem {
	field := constraintFields[e.ConstraintName]
	if field == "" {
		field = e.ColumnName
	}
	switch e.Code {
	case "P0001":
		switch e.Message {
		case CodeInsufficient:
			p := &Problem{Status: http.StatusUnprocessableEntity, Code: CodeInsufficient, Detail: e.Detail}
			var line map[string]any
			if json.Unmarshal([]byte(e.Hint), &line) == nil {
				p.Extra = map[string]any{"short_lines": []any{line}, "alert_sent": false}
			}
			return p
		case "LAST_SUPER_ADMIN":
			return Conflict(CodeConflict, "at least one active Super Admin is required")
		}
		return InvalidState(e.Message)
	case "23505":
		p := Conflict(CodeDuplicate, "a record with the same "+orDefault(field, "values")+" already exists")
		if field != "" {
			p.Errors = []FieldError{{Field: field, Message: "already exists"}}
		}
		return p
	case "23514":
		if e.ConstraintName == "stock_balance_qty_check" {
			return &Problem{Status: http.StatusUnprocessableEntity, Code: CodeInsufficient, Detail: "stock cannot go below zero"}
		}
		return &Problem{Status: http.StatusBadRequest, Code: CodeValidation, Detail: "rule broken: " + e.ConstraintName}
	case "23503":
		return &Problem{Status: http.StatusBadRequest, Code: CodeValidation, Detail: "unknown or still-referenced record (" + e.ConstraintName + ")"}
	case "23502":
		return Validation(e.ColumnName, "is required")
	case "22P02", "22003", "22007", "22008":
		return &Problem{Status: http.StatusBadRequest, Code: CodeValidation, Detail: e.Message}
	case "40001", "40P01":
		return Conflict(CodeConflict, "the record was busy; please retry")
	case "57014":
		return &Problem{Status: http.StatusServiceUnavailable, Code: CodeInternal, Detail: "the database took too long; please retry"}
	case "42501":
		return &Problem{Status: http.StatusForbidden, Code: CodeForbidden, Detail: "this record cannot be changed"}
	}
	return nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// JSON writes v with the given status.
func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// NoContent writes 204.
func NoContent(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }

// ETag sets the bill-version ETag ("3").
func ETag(w http.ResponseWriter, version int32) {
	w.Header().Set("ETag", fmt.Sprintf(`"%d"`, version))
}

// IfMatch reads the required If-Match version (`"3"` or `3`).
func IfMatch(r *http.Request) (int32, error) {
	v := strings.Trim(strings.TrimPrefix(strings.TrimSpace(r.Header.Get("If-Match")), "W/"), `"`)
	if v == "" {
		return 0, Validation("If-Match", "If-Match header with the bill version is required")
	}
	var n int32
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		return 0, Validation("If-Match", "If-Match must be the bill version, e.g. \"3\"")
	}
	return n, nil
}
