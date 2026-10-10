package httpx

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// PageMeta is the "meta" of every list response.
type PageMeta struct {
	Page     int `json:"page"`
	PageSize int `json:"page_size"`
	Total    int `json:"total"`
}

// List is the { data, meta } envelope.
type List[T any] struct {
	Data []T      `json:"data"`
	Meta PageMeta `json:"meta"`
}

// Page holds ?page and ?page_size (default 1 / 50, max 200).
type Page struct {
	Page, Size int
}

func (p Page) Limit() int32  { return int32(p.Size) }
func (p Page) Offset() int32 { return int32((p.Page - 1) * p.Size) }

// NewList builds the envelope; total is the row count before paging.
func NewList[T any](data []T, p Page, total int64) List[T] {
	if data == nil {
		data = []T{}
	}
	return List[T]{Data: data, Meta: PageMeta{Page: p.Page, PageSize: p.Size, Total: int(total)}}
}

func ParsePage(r *http.Request) (Page, error) {
	p := Page{Page: 1, Size: 50}
	if v := r.URL.Query().Get("page"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return p, Validation("page", "must be 1 or more")
		}
		p.Page = n
	}
	if v := r.URL.Query().Get("page_size"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			return p, Validation("page_size", "must be between 1 and 200")
		}
		p.Size = n
	}
	return p, nil
}

// PathID reads a numeric {name} path parameter; anything else is 404.
func PathID(r *http.Request, name string) (int64, error) {
	n, err := strconv.ParseInt(chi.URLParam(r, name), 10, 64)
	if err != nil || n < 1 {
		return 0, NotFound("record")
	}
	return n, nil
}

// PathUUID reads a UUID {name} path parameter; anything else is 404.
func PathUUID(r *http.Request, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, name))
	if err != nil {
		return uuid.Nil, NotFound("record")
	}
	return id, nil
}

// QInt64 reads an optional positive integer query parameter.
func QInt64(r *http.Request, name string) (*int64, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return nil, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 1 {
		return nil, Validation(name, "must be a positive integer")
	}
	return &n, nil
}

// QDate reads an optional YYYY-MM-DD query parameter.
func QDate(r *http.Request, name string) (*time.Time, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return nil, nil
	}
	t, err := time.Parse(time.DateOnly, v)
	if err != nil {
		return nil, Validation(name, "must be a date like 2026-10-10")
	}
	return &t, nil
}

// QBool reads an optional boolean query parameter.
func QBool(r *http.Request, name string) (bool, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, Validation(name, "must be true or false")
	}
	return b, nil
}

// QEnum reads an optional query parameter restricted to allowed values.
func QEnum(r *http.Request, name string, allowed ...string) (*string, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return nil, nil
	}
	for _, a := range allowed {
		if v == a {
			return &v, nil
		}
	}
	return nil, Validation(name, "must be one of: "+strings.Join(allowed, ", "))
}

// QString reads an optional trimmed query parameter.
func QString(r *http.Request, name string) string {
	return strings.TrimSpace(r.URL.Query().Get(name))
}

// Sort parses ?sort=-field against an allow-list; returns field and desc.
func Sort(r *http.Request, def string, allowed ...string) (string, bool, error) {
	v := r.URL.Query().Get("sort")
	if v == "" {
		v = def
	}
	desc := strings.HasPrefix(v, "-")
	f := strings.TrimPrefix(v, "-")
	for _, a := range allowed {
		if f == a {
			return f, desc, nil
		}
	}
	return "", false, Validation("sort", "must be one of: "+strings.Join(allowed, ", ")+" (prefix - for descending)")
}

// Range reads ?from and ?to (both optional) and checks the order.
func Range(r *http.Request) (*time.Time, *time.Time, error) {
	from, err := QDate(r, "from")
	if err != nil {
		return nil, nil, err
	}
	to, err := QDate(r, "to")
	if err != nil {
		return nil, nil, err
	}
	if from != nil && to != nil && to.Before(*from) {
		return nil, nil, Validation("to", "must not be before from")
	}
	return from, to, nil
}
