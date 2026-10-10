package server_test

// End-to-end harness: the real router and job workers against PostgreSQL in
// Docker, with a fake Supabase (JWKS, Auth admin, Storage). Every JSON
// response is validated against openapi.yaml (status code documented for the
// operation, body matches its schema).

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"

	"billing-api/internal/app"
	"billing-api/internal/auth"
	"billing-api/internal/config"
	"billing-api/internal/server"
	"billing-api/internal/testenv"
)

// ---------------------------------------------------------------- fake Supabase

type fakeSupabase struct {
	srv    *httptest.Server
	key    *ecdsa.PrivateKey
	super  *pgxpool.Pool
	mu     sync.Mutex
	files  map[string][]byte
	resets []string
}

func newFakeSupabase(t *testing.T, superURL string) *fakeSupabase {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(context.Background(), superURL)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSupabase{key: key, super: pool, files: map[string][]byte{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/v1/.well-known/jwks.json", f.jwks)
	mux.HandleFunc("POST /auth/v1/invite", f.invite)
	mux.HandleFunc("DELETE /auth/v1/admin/users/{id}", f.deleteUser)
	mux.HandleFunc("POST /auth/v1/recover", f.recover)
	mux.HandleFunc("POST /auth/v1/token", func(w http.ResponseWriter, r *http.Request) { f.signIn(t, w, r) })
	mux.HandleFunc("POST /auth/v1/logout", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("POST /storage/v1/object/sign/{bucket}/{path...}", f.sign)
	mux.HandleFunc("POST /storage/v1/object/{bucket}/{path...}", f.upload)
	mux.HandleFunc("GET /storage/v1/object/{bucket}/{path...}", f.download)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(func() { f.srv.Close(); pool.Close() })
	return f
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (f *fakeSupabase) jwks(w http.ResponseWriter, _ *http.Request) {
	pub := f.key.PublicKey
	x, y := make([]byte, 32), make([]byte, 32)
	pub.X.FillBytes(x)
	pub.Y.FillBytes(y)
	_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
		"kty": "EC", "crv": "P-256", "x": b64(x), "y": b64(y), "kid": "test", "alg": "ES256", "use": "sig"}}})
}

func (f *fakeSupabase) token(t *testing.T, sub uuid.UUID, session string) string {
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
		"sub": sub.String(), "aud": "authenticated", "iss": f.srv.URL + "/auth/v1", "role": "authenticated",
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "session_id": session,
	})
	tok.Header["kid"] = "test"
	s, err := tok.SignedString(f.key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// signIn implements the password and refresh_token grants (any password).
func (f *fakeSupabase) signIn(t *testing.T, w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email        string `json:"email"`
		RefreshToken string `json:"refresh_token"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	var id uuid.UUID
	switch r.URL.Query().Get("grant_type") {
	case "password":
		if err := f.super.QueryRow(r.Context(), `select id from auth.users where email = $1`, in.Email).Scan(&id); err != nil {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
	case "refresh_token":
		var err error
		if id, err = uuid.Parse(strings.TrimPrefix(in.RefreshToken, "r-")); err != nil {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
	default:
		http.Error(w, `{"error":"unsupported_grant_type"}`, http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": f.token(t, id, uuid.NewString()), "token_type": "bearer",
		"expires_in": 3600, "refresh_token": "r-" + id.String(), "user": map[string]any{"id": id, "email": in.Email}})
}

func (f *fakeSupabase) invite(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email string `json:"email"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	var exists bool
	_ = f.super.QueryRow(r.Context(), `select exists(select 1 from auth.users where email = $1)`, in.Email).Scan(&exists)
	if exists {
		http.Error(w, `{"msg":"A user with this email address has already been registered"}`, http.StatusUnprocessableEntity)
		return
	}
	id := uuid.New()
	if _, err := f.super.Exec(r.Context(), `insert into auth.users (id, email) values ($1, $2)`, id, in.Email); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "email": in.Email})
}

func (f *fakeSupabase) deleteUser(w http.ResponseWriter, r *http.Request) {
	_, _ = f.super.Exec(r.Context(), `delete from auth.users where id = $1`, r.PathValue("id"))
	w.WriteHeader(http.StatusOK)
}

func (f *fakeSupabase) recover(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email string `json:"email"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	f.mu.Lock()
	f.resets = append(f.resets, in.Email)
	f.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (f *fakeSupabase) upload(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.files[r.PathValue("path")] = b
	f.mu.Unlock()
	_ = json.NewEncoder(w).Encode(map[string]string{"Key": r.PathValue("path")})
}

func (f *fakeSupabase) download(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	b, ok := f.files[r.PathValue("path")]
	f.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	_, _ = w.Write(b)
}

func (f *fakeSupabase) sign(w http.ResponseWriter, r *http.Request) {
	p := r.PathValue("path")
	f.mu.Lock()
	_, ok := f.files[p]
	f.mu.Unlock()
	if !ok {
		http.Error(w, `{"error":"not found"}`, http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"signedURL": "/object/sign/" + r.PathValue("bucket") + "/" + p + "?token=t"})
}

// file returns an uploaded file by the path inside a signed URL.
func (f *fakeSupabase) file(t *testing.T, signedURL string) []byte {
	u, err := url.Parse(signedURL)
	if err != nil {
		t.Fatal(err)
	}
	p := strings.TrimPrefix(u.Path, "/storage/v1/object/sign/documents/")
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.files[p]
	if !ok {
		t.Fatalf("no stored file for %s", signedURL)
	}
	return b
}

// ---------------------------------------------------------------- contract

type contract struct {
	compiler *jsonschema.Compiler
	spec     map[string]any
	ops      []specOp
	cache    map[string]*jsonschema.Schema
	hit      map[string]bool
	mu       sync.Mutex
}

type specOp struct {
	tmpl string
	re   *regexp.Regexp
}

func loadContract(t *testing.T) *contract {
	raw, err := os.ReadFile(filepath.Join(testenv.RepoRoot(), "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var spec map[string]any
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	js, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(js))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	if err := c.AddResource("spec.json", doc); err != nil {
		t.Fatal(err)
	}
	ct := &contract{compiler: c, spec: spec, cache: map[string]*jsonschema.Schema{}, hit: map[string]bool{}}
	for p := range spec["paths"].(map[string]any) {
		segs := strings.Split(p, "/")
		for i, s := range segs {
			if strings.HasPrefix(s, "{") {
				segs[i] = "[^/]+"
			} else {
				segs[i] = regexp.QuoteMeta(s)
			}
		}
		ct.ops = append(ct.ops, specOp{tmpl: p, re: regexp.MustCompile("^" + strings.Join(segs, "/") + "$")})
	}
	// Literal segments win over templates (/bills/calculate vs /bills/{id}).
	sort.Slice(ct.ops, func(i, j int) bool { return strings.Count(ct.ops[i].tmpl, "{") < strings.Count(ct.ops[j].tmpl, "{") })
	return ct
}

func esc(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1") }

// check validates one response against the spec.
func (c *contract) check(t *testing.T, method, path string, status int, contentType string, body []byte) {
	t.Helper()
	path = strings.TrimPrefix(path, "/api/v1")
	var tmpl string
	for _, o := range c.ops {
		if o.re.MatchString(path) {
			tmpl = o.tmpl
			break
		}
	}
	if tmpl == "" {
		t.Errorf("contract: %s %s is not in openapi.yaml", method, path)
		return
	}
	c.mu.Lock()
	c.hit[strings.ToUpper(method)+" "+tmpl] = true
	c.mu.Unlock()
	op, ok := c.spec["paths"].(map[string]any)[tmpl].(map[string]any)[strings.ToLower(method)].(map[string]any)
	if !ok {
		t.Errorf("contract: %s %s not documented", method, tmpl)
		return
	}
	code := fmt.Sprint(status)
	resp, ok := op["responses"].(map[string]any)[code].(map[string]any)
	if !ok {
		t.Errorf("contract: %s %s returned %d, which the spec does not document (body %s)", method, tmpl, status, trunc(body))
		return
	}
	pointer := "/paths/" + esc(tmpl) + "/" + strings.ToLower(method) + "/responses/" + code
	if ref, ok := resp["$ref"].(string); ok {
		name := strings.TrimPrefix(ref, "#/components/responses/")
		resp = c.spec["components"].(map[string]any)["responses"].(map[string]any)[name].(map[string]any)
		pointer = "/components/responses/" + esc(name)
	}
	content, _ := resp["content"].(map[string]any)
	if len(content) == 0 || len(bytes.TrimSpace(body)) == 0 {
		return
	}
	ct := "application/json"
	if _, ok := content[ct]; !ok {
		ct = "application/problem+json"
	}
	if _, ok := content[ct]; !ok {
		return // binary download
	}
	if !strings.HasPrefix(contentType, "application/json") && !strings.HasPrefix(contentType, "application/problem+json") {
		t.Errorf("contract: %s %s %d: content type %q", method, tmpl, status, contentType)
		return
	}
	loc := "spec.json#" + pointer + "/content/" + esc(ct) + "/schema"
	c.mu.Lock()
	sch, ok := c.cache[loc]
	if !ok {
		var err error
		if sch, err = c.compiler.Compile(loc); err != nil {
			c.mu.Unlock()
			t.Fatalf("contract: compile %s: %v", loc, err)
		}
		c.cache[loc] = sch
	}
	c.mu.Unlock()
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		t.Errorf("contract: %s %s: invalid JSON: %v", method, tmpl, err)
		return
	}
	if err := sch.Validate(inst); err != nil {
		t.Errorf("contract: %s %s %d does not match the spec:\n%v\nbody: %s", method, tmpl, status, err, trunc(body))
	}
}

func trunc(b []byte) string {
	if len(b) > 1500 {
		return string(b[:1500]) + "..."
	}
	return string(b)
}

// ---------------------------------------------------------------- environment

type clock struct{ offset atomic.Int64 }

func (c *clock) Now() time.Time          { return time.Now().Add(time.Duration(c.offset.Load())) }
func (c *clock) Advance(d time.Duration) { c.offset.Add(int64(d)) }
func (c *clock) Reset()                  { c.offset.Store(0) }

type env struct {
	t        *testing.T
	api      *httptest.Server
	sb       *fakeSupabase
	app      *app.App
	db       *testenv.DB
	super    *pgxpool.Pool
	contract *contract
	clock    *clock
}

func startEnv(t *testing.T) *env {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := testenv.Start(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	sb := newFakeSupabase(t, db.SuperURL)
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	cfg := config.Config{Env: "development", Port: "0", DatabaseURL: db.APIURL, SupabaseURL: sb.srv.URL,
		ServiceRoleKey: "service-role", StorageBucket: "documents", EncryptionKey: base64.StdEncoding.EncodeToString(key),
		AllowedOrigins: []string{"http://localhost:5173"}, RunAPI: true, RunWorker: true, MigrateRiver: true,
		LogLevel: slog.LevelWarn}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if os.Getenv("TEST_LOG") != "" {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	slog.SetDefault(logger)
	a, err := server.Build(context.Background(), cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{}
	a.Now = clk.Now
	if err := a.River.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = a.River.Stop(c)
		a.Pool.Close()
	})
	v, err := auth.NewVerifier(context.Background(), cfg.JWKSURL(), cfg.JWTIssuer())
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(server.Router(a, v))
	t.Cleanup(api.Close)
	super, err := pgxpool.New(context.Background(), db.SuperURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(super.Close)
	return &env{t: t, api: api, sb: sb, app: a, db: db, super: super, contract: loadContract(t), clock: clk}
}

// owner creates the first Super Admin directly (as in the README bootstrap).
func (e *env) owner() *client {
	id := uuid.New()
	ctx := context.Background()
	if _, err := e.super.Exec(ctx, `insert into auth.users (id, email) values ($1, 'owner@shop.in')`, id); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.super.Exec(ctx, `insert into app.app_user (id, full_name, email, role) values ($1, 'Owner', 'owner@shop.in', 'super_admin')`, id); err != nil {
		e.t.Fatal(err)
	}
	return e.client(id)
}

func (e *env) client(id uuid.UUID) *client {
	return &client{e: e, id: id, token: e.sb.token(e.t, id, uuid.NewString())}
}

// ---------------------------------------------------------------- client

type client struct {
	e     *env
	id    uuid.UUID
	token string
}

type resp struct {
	t      *testing.T
	method string
	path   string
	status int
	body   []byte
	header http.Header
}

func (c *client) do(t *testing.T, method, path string, body any, hdr map[string]string) *resp {
	t.Helper()
	var rd io.Reader
	ctype := ""
	switch b := body.(type) {
	case nil:
	case *multipart.Writer:
		t.Fatal("use upload")
	case []byte:
		rd = bytes.NewReader(b)
		ctype = "application/json"
	default:
		js, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(js)
		ctype = "application/json"
	}
	req, err := http.NewRequest(method, c.e.api.URL+"/api/v1"+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if method == http.MethodPost {
		req.Header.Set("Idempotency-Key", uuid.NewString())
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return c.send(t, req)
}

func (c *client) send(t *testing.T, req *http.Request) *resp {
	t.Helper()
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	r := &resp{t: t, method: req.Method, path: req.URL.Path, status: res.StatusCode, body: b, header: res.Header}
	c.e.contract.check(t, req.Method, req.URL.Path, res.StatusCode, res.Header.Get("Content-Type"), b)
	return r
}

// upload posts a multipart form.
func (c *client) upload(t *testing.T, method, path string, fields map[string][]string, fileField, fileName string, data []byte) *resp {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, vs := range fields {
		for _, v := range vs {
			_ = mw.WriteField(k, v)
		}
	}
	fw, _ := mw.CreateFormFile(fileField, fileName)
	_, _ = fw.Write(data)
	_ = mw.Close()
	req, _ := http.NewRequest(method, c.e.api.URL+"/api/v1"+path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Idempotency-Key", uuid.NewString())
	return c.send(t, req)
}

func (c *client) get(t *testing.T, path string) *resp {
	t.Helper()
	return c.do(t, http.MethodGet, path, nil, nil)
}
func (c *client) post(t *testing.T, path string, body any) *resp {
	t.Helper()
	return c.do(t, http.MethodPost, path, body, nil)
}
func (c *client) patch(t *testing.T, path string, body any) *resp {
	t.Helper()
	return c.do(t, http.MethodPatch, path, body, nil)
}
func (c *client) put(t *testing.T, path string, body any) *resp {
	t.Helper()
	return c.do(t, http.MethodPut, path, body, nil)
}

func (r *resp) expect(status int) *resp {
	r.t.Helper()
	if r.status != status {
		r.t.Fatalf("%s %s: status %d, want %d\n%s", r.method, r.path, r.status, status, trunc(r.body))
	}
	return r
}

func (r *resp) code(code string) *resp {
	r.t.Helper()
	var p struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(r.body, &p)
	if p.Code != code {
		r.t.Fatalf("%s %s: code %q, want %q\n%s", r.method, r.path, p.Code, code, trunc(r.body))
	}
	return r
}

func (r *resp) into(v any) *resp {
	r.t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		r.t.Fatalf("decode %s: %v\n%s", r.path, err, trunc(r.body))
	}
	return r
}

// m decodes into a generic map.
func (r *resp) m() map[string]any {
	r.t.Helper()
	var v map[string]any
	r.into(&v)
	return v
}

// eventually polls cond for up to 15 s.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func str(v any) string { return fmt.Sprint(v) }

// uncovered lists spec operations no test request reached.
func (c *contract) uncovered() []string {
	var out []string
	for p, item := range c.spec["paths"].(map[string]any) {
		for m := range item.(map[string]any) {
			key := strings.ToUpper(m) + " " + p
			if !c.hit[key] {
				out = append(out, key)
			}
		}
	}
	sort.Strings(out)
	return out
}
