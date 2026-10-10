// Package supabase calls Supabase Auth (admin) and Storage with the
// service-role key. The key never leaves the server (HLD 6.7).
package supabase

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrEmailExists is returned by Invite when the email is already registered.
var ErrEmailExists = errors.New("a user with this email already exists")

// Storage is the file store for PDFs, logos and exports (private bucket).
type Storage interface {
	Upload(ctx context.Context, path, contentType string, data []byte) error
	Download(ctx context.Context, path string) ([]byte, error)
	SignedURL(ctx context.Context, path string, ttl time.Duration) (string, time.Time, error)
}

// AuthAdmin manages Supabase Auth users.
type AuthAdmin interface {
	Invite(ctx context.Context, email string, meta map[string]any) (uuid.UUID, error)
	DeleteUser(ctx context.Context, id uuid.UUID) error
	SendPasswordReset(ctx context.Context, email string) error
}

// Client implements Storage and AuthAdmin over HTTP.
type Client struct {
	baseURL    string
	serviceKey string
	bucket     string
	redirectTo string
	http       *http.Client
}

func New(baseURL, serviceKey, bucket, redirectTo string) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		serviceKey: serviceKey,
		bucket:     bucket,
		redirectTo: redirectTo,
		http:       &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) do(ctx context.Context, method, path, contentType string, body []byte, out any) (int, error) {
	if c.serviceKey == "" {
		return 0, errors.New("SUPABASE_SERVICE_ROLE_KEY is not configured")
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("apikey", c.serviceKey)
	req.Header.Set("Authorization", "Bearer "+c.serviceKey)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 50<<20))
	if resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("supabase %s %s: %d %s", method, path, resp.StatusCode, truncate(string(b), 300))
	}
	if out != nil {
		switch o := out.(type) {
		case *[]byte:
			*o = b
		default:
			if err := json.Unmarshal(b, out); err != nil {
				return resp.StatusCode, fmt.Errorf("supabase %s %s: decode: %w", method, path, err)
			}
		}
	}
	return resp.StatusCode, nil
}

func jsonBody(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// ---------------------------------------------------------------- auth admin

func (c *Client) Invite(ctx context.Context, email string, meta map[string]any) (uuid.UUID, error) {
	path := "/auth/v1/invite"
	if c.redirectTo != "" {
		path += "?redirect_to=" + url.QueryEscape(c.redirectTo)
	}
	var user struct {
		ID uuid.UUID `json:"id"`
	}
	status, err := c.do(ctx, http.MethodPost, path, "application/json",
		jsonBody(map[string]any{"email": email, "data": meta}), &user)
	if err != nil {
		if status == http.StatusUnprocessableEntity || status == http.StatusConflict {
			return uuid.Nil, ErrEmailExists
		}
		return uuid.Nil, err
	}
	return user.ID, nil
}

func (c *Client) DeleteUser(ctx context.Context, id uuid.UUID) error {
	_, err := c.do(ctx, http.MethodDelete, "/auth/v1/admin/users/"+id.String(), "", nil, nil)
	return err
}

func (c *Client) SendPasswordReset(ctx context.Context, email string) error {
	path := "/auth/v1/recover"
	if c.redirectTo != "" {
		path += "?redirect_to=" + url.QueryEscape(c.redirectTo)
	}
	_, err := c.do(ctx, http.MethodPost, path, "application/json", jsonBody(map[string]string{"email": email}), nil)
	return err
}

// ---------------------------------------------------------------- storage

func (c *Client) objectPath(p string) string {
	return "/storage/v1/object/" + c.bucket + "/" + strings.TrimLeft(p, "/")
}

func (c *Client) Upload(ctx context.Context, path, contentType string, data []byte) error {
	if c.serviceKey == "" {
		return errors.New("SUPABASE_SERVICE_ROLE_KEY is not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+c.objectPath(path), bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("apikey", c.serviceKey)
	req.Header.Set("Authorization", "Bearer "+c.serviceKey)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("x-upsert", "true")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1000))
		return fmt.Errorf("storage upload %s: %d %s", path, resp.StatusCode, truncate(string(b), 300))
	}
	return nil
}

func (c *Client) Download(ctx context.Context, path string) ([]byte, error) {
	var b []byte
	_, err := c.do(ctx, http.MethodGet, c.objectPath(path), "", nil, &b)
	return b, err
}

func (c *Client) SignedURL(ctx context.Context, path string, ttl time.Duration) (string, time.Time, error) {
	var out struct {
		SignedURL string `json:"signedURL"`
	}
	sec := int(ttl.Seconds())
	_, err := c.do(ctx, http.MethodPost, "/storage/v1/object/sign/"+c.bucket+"/"+strings.TrimLeft(path, "/"),
		"application/json", jsonBody(map[string]int{"expiresIn": sec}), &out)
	if err != nil {
		return "", time.Time{}, err
	}
	return c.baseURL + "/storage/v1" + out.SignedURL, time.Now().Add(ttl).UTC().Truncate(time.Second), nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
