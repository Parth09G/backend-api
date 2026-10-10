package server_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"billing-api/internal/testenv"
)

// TestPostmanCollection runs postman/billing-api.postman_collection.json with
// newman against a fresh server, so the collection stays runnable as the
// API changes. Needs Docker and Node (npx); skipped otherwise.
func TestPostmanCollection(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: needs Docker")
	}
	npx, err := exec.LookPath("npx")
	if err != nil {
		t.Skip("npx not found; install Node.js to run the Postman collection")
	}
	e := startEnv(t)
	e.owner()

	dir := filepath.Join(testenv.RepoRoot(), "postman")
	env := map[string]any{"name": "test", "values": []map[string]any{
		{"key": "api_url", "value": e.api.URL, "enabled": true},
		{"key": "supabase_url", "value": e.sb.srv.URL, "enabled": true},
		{"key": "supabase_anon_key", "value": "anon", "enabled": true},
		{"key": "email", "value": "owner@shop.in", "enabled": true},
		{"key": "password", "value": "any", "enabled": true},
	}}
	b, _ := json.Marshal(env)
	envFile := filepath.Join(t.TempDir(), "env.json")
	if err := os.WriteFile(envFile, b, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(npx, "--yes", "newman@6", "run", filepath.Join(dir, "billing-api.postman_collection.json"),
		"-e", envFile, "--working-dir", dir, "--reporters", "cli", "--color", "off", "--delay-request", "100")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("newman failed: %v\n%s", err, tail(string(out), 6000))
	}
	if !strings.Contains(string(out), "assertions") {
		t.Fatalf("unexpected newman output:\n%s", tail(string(out), 3000))
	}
	t.Logf("%s", tail(string(out), 1500))
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
