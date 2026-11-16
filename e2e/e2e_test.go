package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"
)

// The suite runs the real binary against real Postgres and Redis. Nothing is
// mocked; the point is to catch wiring mistakes that unit tests cannot see.

const (
	mainKey  = "e2e-main-key-0123456789"
	limitKey = "e2e-limit-key-0123456789"
	pepper   = "e2e-pepper"
)

var (
	databaseURL = envOr("E2E_DATABASE_URL", "postgres://shortener:shortener@localhost:5432/shortener?sslmode=disable")
	redisAddr   = envOr("E2E_REDIS_ADDR", "localhost:6379")

	// runTag keeps fixed scenario codes unique per run, so the suite can be
	// re-run against a dirty database without a reset.
	runTag = fmt.Sprintf("%x", time.Now().UnixNano()&0xfffff)

	binPath string
	workDir string
	baseURL string

	appCmd *exec.Cmd
	appLog *os.File

	db  *pgxpool.Pool
	rdb *goredis.Client

	resultsMu sync.Mutex
	results   []scenarioResult
)

type scenarioResult struct {
	Name     string        `json:"name"`
	Passed   bool          `json:"passed"`
	Duration time.Duration `json:"duration_ns"`
	Evidence []string      `json:"evidence"`
}

var httpc = &http.Client{
	Timeout: 5 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// TestMain builds the API, starts it, runs the scenarios, then writes the
// artifact. Postgres and Redis are expected to be up (scripts/e2e.ps1 does it).
func TestMain(m *testing.M) {
	code := 0
	if err := setup(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e setup failed: %v\n", err)
		code = 1
	} else {
		code = m.Run()
	}
	teardown()
	writeReport()
	os.Exit(code)
}

func setup() error {
	var err error
	workDir, err = os.MkdirTemp("", "shortener-e2e-*")
	if err != nil {
		return err
	}

	binPath = filepath.Join(workDir, "shortener.exe")
	build := exec.Command("go", "build", "-o", binPath, "./cmd/api")
	build.Dir = ".."
	if out, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("build api: %v\n%s", err, out)
	}

	appLog, err = os.OpenFile(filepath.Join(workDir, "app.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}

	db, err = pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.Ping(pingCtx); err != nil {
		return fmt.Errorf("ping postgres (is docker compose up?): %w", err)
	}

	rdb = goredis.NewClient(&goredis.Options{Addr: redisAddr})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		return fmt.Errorf("ping redis (is docker compose up?): %w", err)
	}

	return startApp()
}

func startApp() error {
	port, err := freePort()
	if err != nil {
		return err
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	baseURL = "http://" + addr

	appCmd = exec.Command(binPath)
	appCmd.Stdout = appLog
	appCmd.Stderr = appLog
	appCmd.Env = append(os.Environ(),
		"APP_ENV=test",
		"HTTP_ADDR="+addr,
		"PUBLIC_BASE_URL="+baseURL,
		"DATABASE_URL="+databaseURL,
		"REDIS_ADDR="+redisAddr,
		"API_KEY_PEPPER="+pepper,
		"BOOTSTRAP_USERS=main:"+mainKey+",limit:"+limitKey,
		"RATE_LIMIT_PER_MINUTE=100",
		"CACHE_TTL=24h",
		"NEGATIVE_CACHE_TTL=30s",
		"CLICK_BUFFER=4096",
		"ALLOW_PRIVATE_TARGETS=false",
		"TRUSTED_PROXY_CIDRS=127.0.0.1/32,::1/128",
		"LOG_LEVEL=error",
	)
	if err := appCmd.Start(); err != nil {
		return fmt.Errorf("start api: %w", err)
	}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := httpc.Get(baseURL + "/readyz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("api did not become ready; log:\n%s", tailLog())
}

func teardown() {
	stopApp()
	if db != nil {
		db.Close()
	}
	if rdb != nil {
		_ = rdb.Close()
	}
	if appLog != nil {
		_ = appLog.Close()
	}
}

func stopApp() {
	if appCmd == nil || appCmd.Process == nil {
		return
	}
	_ = appCmd.Process.Kill()
	_, _ = appCmd.Process.Wait()
	appCmd = nil
}

func restart(t *testing.T) {
	t.Helper()
	oldPID := appCmd.Process.Pid
	stopApp()
	if err := startApp(); err != nil {
		t.Fatalf("restart api: %v", err)
	}
	t.Logf("restarted api: pid %d -> %d at %s", oldPID, appCmd.Process.Pid, baseURL)
}

// --- test plumbing ------------------------------------------------------

type evidence struct {
	lines []string
}

func (e *evidence) add(format string, args ...any) {
	e.lines = append(e.lines, fmt.Sprintf(format, args...))
}

// newEvidence registers a cleanup that records the scenario outcome.
func newEvidence(t *testing.T) *evidence {
	t.Helper()
	e := &evidence{}
	start := time.Now()
	t.Cleanup(func() {
		resultsMu.Lock()
		results = append(results, scenarioResult{
			Name:     t.Name(),
			Passed:   !t.Failed(),
			Duration: time.Since(start),
			Evidence: e.lines,
		})
		resultsMu.Unlock()
	})
	return e
}

type createResponse struct {
	Code      string     `json:"code"`
	ShortURL  string     `json:"short_url"`
	TargetURL string     `json:"target_url"`
	ExpiresAt *time.Time `json:"expires_at"`
	Expired   bool       `json:"expired"`
	Clicks    int64      `json:"clicks"`
}

func request(t *testing.T, method, path, key string, body any) (*http.Response, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, baseURL+path, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpc.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, raw
}

func mustStatus(t *testing.T, resp *http.Response, body []byte, want int) {
	t.Helper()
	if resp.StatusCode != want {
		t.Fatalf("status = %d, want %d (body: %s)", resp.StatusCode, want, truncate(body))
	}
}

func createLink(t *testing.T, key, target, custom string) createResponse {
	t.Helper()
	payload := map[string]any{"target_url": target}
	if custom != "" {
		payload["custom_code"] = custom
	}
	resp, body := request(t, http.MethodPost, "/api/v1/links", key, payload)
	mustStatus(t, resp, body, http.StatusCreated)
	var out createResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode create response: %v (%s)", err, truncate(body))
	}
	return out
}

func resolve(t *testing.T, code string) (*http.Response, []byte) {
	t.Helper()
	return request(t, http.MethodGet, "/"+code, "", nil)
}

func waitFor(t *testing.T, timeout time.Duration, what string, fn func() (bool, error)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ok, err := fn()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if ok {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func truncate(b []byte) string {
	const max = 400
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

func tailLog() string {
	if appLog == nil {
		return ""
	}
	raw, err := os.ReadFile(appLog.Name())
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) > 15 {
		lines = lines[len(lines)-15:]
	}
	return strings.Join(lines, "\n")
}

// --- scenarios ----------------------------------------------------------

func TestHealthAndReadiness(t *testing.T) {
	ev := newEvidence(t)

	resp, body := request(t, http.MethodGet, "/healthz", "", nil)
	mustStatus(t, resp, body, http.StatusOK)
	ev.add("GET /healthz -> %d %s", resp.StatusCode, truncate(body))

	resp, body = request(t, http.MethodGet, "/readyz", "", nil)
	mustStatus(t, resp, body, http.StatusOK)
	ev.add("GET /readyz -> %d %s", resp.StatusCode, truncate(body))
}

func TestCreateAndRedirect(t *testing.T) {
	ev := newEvidence(t)
	target := "https://example.com/a/very/long/path?utm_source=e2e&x=1"

	created := createLink(t, mainKey, target, "")
	if len(created.Code) != 7 {
		t.Fatalf("code %q should be 7 characters", created.Code)
	}
	if !isBase62(created.Code) {
		t.Fatalf("code %q is not base62", created.Code)
	}
	if want := baseURL + "/" + created.Code; created.ShortURL != want {
		t.Fatalf("short_url = %q, want %q", created.ShortURL, want)
	}
	ev.add("created code %s -> %s", created.Code, created.TargetURL)

	resp, body := resolve(t, created.Code)
	mustStatus(t, resp, body, http.StatusFound)
	if got := resp.Header.Get("Location"); got != target {
		t.Fatalf("Location = %q, want %q", got, target)
	}
	ev.add("GET /%s -> 302 Location: %s", created.Code, resp.Header.Get("Location"))

	exists, err := rdb.Exists(context.Background(), "link:"+created.Code).Result()
	if err != nil {
		t.Fatalf("redis exists: %v", err)
	}
	if exists != 1 {
		t.Fatalf("link:%s should be cached after creation", created.Code)
	}
	ev.add("redis link:%s exists = %d", created.Code, exists)
}

func TestCacheServesStaleRow(t *testing.T) {
	ev := newEvidence(t)
	code := "cache" + runTag
	original := "https://example.com/original"
	changed := "https://example.org/changed"

	createLink(t, mainKey, original, code)

	if err := rdb.Del(context.Background(), "link:"+code).Err(); err != nil {
		t.Fatalf("clear cache: %v", err)
	}
	resp, body := resolve(t, code)
	mustStatus(t, resp, body, http.StatusFound)
	if got := resp.Header.Get("Location"); got != original {
		t.Fatalf("Location = %q, want %q", got, original)
	}

	// Change the row behind the cache. A cache hit must keep serving the old
	// target; that is the only way to prove the read path is not touching
	// Postgres on every redirect.
	if _, err := db.Exec(context.Background(), `UPDATE links SET target_url = $2 WHERE code = $1`, code, changed); err != nil {
		t.Fatalf("update row: %v", err)
	}
	resp, body = resolve(t, code)
	mustStatus(t, resp, body, http.StatusFound)
	if got := resp.Header.Get("Location"); got != original {
		t.Fatalf("cache hit returned %q, want cached %q", got, original)
	}
	ev.add("row changed to %s; cached redirect still returned %s", changed, original)

	// Dropping the key proves the database holds the new value.
	if err := rdb.Del(context.Background(), "link:"+code).Err(); err != nil {
		t.Fatalf("clear cache: %v", err)
	}
	resp, body = resolve(t, code)
	mustStatus(t, resp, body, http.StatusFound)
	if got := resp.Header.Get("Location"); got != changed {
		t.Fatalf("after eviction Location = %q, want %q", got, changed)
	}
	ev.add("after eviction redirect returned %s", changed)

	if _, err := db.Exec(context.Background(), `DELETE FROM links WHERE code = $1`, code); err != nil {
		t.Fatalf("cleanup row: %v", err)
	}
	_ = rdb.Del(context.Background(), "link:"+code)
}

func TestClickIsRecorded(t *testing.T) {
	ev := newEvidence(t)
	code := "click" + runTag

	createLink(t, mainKey, "https://example.com/click", code)
	resp, body := resolve(t, code)
	mustStatus(t, resp, body, http.StatusFound)

	var clicks int64
	waitFor(t, 5*time.Second, "click counter", func() (bool, error) {
		return db.QueryRow(context.Background(),
			`SELECT clicks FROM links WHERE code = $1`, code).Scan(&clicks) == nil && clicks >= 1, nil
	})
	ev.add("links.clicks = %d", clicks)

	var events int64
	waitFor(t, 5*time.Second, "click event", func() (bool, error) {
		err := db.QueryRow(context.Background(),
			`SELECT count(*) FROM click_events WHERE code = $1`, code).Scan(&events)
		return events >= 1, err
	})
	ev.add("click_events rows = %d (asynchronous write)", events)

	var ua, visitor string
	if err := db.QueryRow(context.Background(),
		`SELECT user_agent, visitor_hash FROM click_events WHERE code = $1 ORDER BY id DESC LIMIT 1`, code).
		Scan(&ua, &visitor); err != nil {
		t.Fatalf("read event: %v", err)
	}
	if visitor == "" {
		t.Fatal("visitor_hash should be populated")
	}
	ev.add("stored user_agent=%q visitor_hash=%s", ua, visitor)
}

// TestForwardedClientIP proves the service reads the caller address from
// X-Forwarded-For when, and only when, the TCP peer is a trusted proxy.
// Otherwise every visitor behind a load balancer would hash identically.
func TestForwardedClientIP(t *testing.T) {
	ev := newEvidence(t)
	code := "fwd" + runTag
	createLink(t, mainKey, "https://example.com/forwarded", code)

	for _, ip := range []string{"203.0.113.7", "198.51.100.9"} {
		req, err := http.NewRequest(http.MethodGet, baseURL+"/"+code, nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.Header.Set("X-Forwarded-For", ip)
		resp, err := httpc.Do(req)
		if err != nil {
			t.Fatalf("redirect: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("status = %d, want 302", resp.StatusCode)
		}
	}

	var hashes []string
	waitFor(t, 5*time.Second, "two click events", func() (bool, error) {
		rows, err := db.Query(context.Background(),
			`SELECT visitor_hash FROM click_events WHERE code = $1`, code)
		if err != nil {
			return false, err
		}
		defer rows.Close()

		hashes = hashes[:0]
		for rows.Next() {
			var h string
			if err := rows.Scan(&h); err != nil {
				return false, err
			}
			hashes = append(hashes, h)
		}
		return len(hashes) >= 2, rows.Err()
	})

	distinct := make(map[string]struct{}, len(hashes))
	for _, h := range hashes {
		distinct[h] = struct{}{}
	}
	if len(distinct) < 2 {
		t.Fatalf("visitor hashes %v are identical; X-Forwarded-For was ignored", hashes)
	}
	ev.add("two forwarded addresses produced %d distinct visitor hashes", len(distinct))
}

func TestNotFoundAndNegativeCache(t *testing.T) {
	ev := newEvidence(t)
	code := "ZZZZZZZ"

	resp, body := resolve(t, code)
	mustStatus(t, resp, body, http.StatusNotFound)
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("404 content-type = %q, want html", ct)
	}
	ev.add("GET /%s -> 404 %s", code, resp.Header.Get("Content-Type"))

	exists, err := rdb.Exists(context.Background(), "miss:"+code).Result()
	if err != nil {
		t.Fatalf("redis exists: %v", err)
	}
	if exists != 1 {
		t.Fatalf("miss:%s should be negatively cached", code)
	}
	ev.add("redis miss:%s exists = %d", code, exists)
}

func TestValidation(t *testing.T) {
	ev := newEvidence(t)

	cases := []struct {
		name    string
		payload map[string]any
	}{
		{"missing target", map[string]any{}},
		{"bad scheme", map[string]any{"target_url": "ftp://example.com/file"}},
		{"not a url", map[string]any{"target_url": "just some text"}},
		{"loopback target", map[string]any{"target_url": "http://127.0.0.1/admin"}},
		{"short custom code", map[string]any{"target_url": "https://example.com/x", "custom_code": "ab"}},
		{"reserved code", map[string]any{"target_url": "https://example.com/x", "custom_code": "api"}},
		{"past expiry", map[string]any{"target_url": "https://example.com/x", "expires_at": time.Now().Add(-time.Hour).Format(time.RFC3339)}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := request(t, http.MethodPost, "/api/v1/links", mainKey, tc.payload)
			mustStatus(t, resp, body, http.StatusBadRequest)
			var e struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(body, &e); err != nil {
				t.Fatalf("decode error body: %v (%s)", err, truncate(body))
			}
			if e.Error.Code != "invalid_request" {
				t.Fatalf("error code = %q, want invalid_request", e.Error.Code)
			}
			ev.add("%s -> 400 (%s)", tc.name, firstLine(body))
		})
	}
}

func TestCustomCodeConflict(t *testing.T) {
	ev := newEvidence(t)
	code := "launch" + runTag

	first := createLink(t, mainKey, "https://example.com/launch", code)
	if first.Code != code {
		t.Fatalf("code = %q, want %q", first.Code, code)
	}
	ev.add("custom alias %s accepted", code)

	resp, body := request(t, http.MethodPost, "/api/v1/links", mainKey, map[string]any{
		"target_url": "https://example.com/other", "custom_code": code,
	})
	mustStatus(t, resp, body, http.StatusConflict)
	ev.add("duplicate alias -> 409 %s", truncate(body))

	resp, body = resolve(t, code)
	mustStatus(t, resp, body, http.StatusFound)
	ev.add("alias still resolves to %s", resp.Header.Get("Location"))
}

func TestOwnershipIsolation(t *testing.T) {
	ev := newEvidence(t)
	code := "secret" + runTag

	createLink(t, mainKey, "https://example.com/secret", code)
	ev.add("user A created %s", code)

	resp, body := request(t, http.MethodGet, "/api/v1/links/"+code, limitKey, nil)
	mustStatus(t, resp, body, http.StatusNotFound)
	ev.add("user B GET api -> 404")

	resp, body = request(t, http.MethodDelete, "/api/v1/links/"+code, limitKey, nil)
	mustStatus(t, resp, body, http.StatusNotFound)
	ev.add("user B DELETE -> 404")

	resp, body = request(t, http.MethodGet, "/api/v1/links/"+code, mainKey, nil)
	mustStatus(t, resp, body, http.StatusOK)

	resp, body = resolve(t, code)
	mustStatus(t, resp, body, http.StatusFound)
	ev.add("owner still resolves; B could not delete it")
}

func TestExpiredLinkIsGone(t *testing.T) {
	ev := newEvidence(t)
	code := "expired1"

	var userID string
	if err := db.QueryRow(context.Background(),
		`SELECT id FROM users WHERE name = 'main'`).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := db.Exec(context.Background(), `
		INSERT INTO links (code, target_url, user_id, created_at, expires_at)
		VALUES ($1, $2, $3, now() - interval '2 hours', now() - interval '1 hour')
		ON CONFLICT (code) DO UPDATE SET expires_at = EXCLUDED.expires_at`,
		code, "https://example.com/expired", userID); err != nil {
		t.Fatalf("insert expired link: %v", err)
	}
	_ = rdb.Del(context.Background(), "link:"+code)
	ev.add("inserted %s with expires_at one hour ago", code)

	resp, body := resolve(t, code)
	mustStatus(t, resp, body, http.StatusNotFound)
	ev.add("GET /%s -> 404", code)

	exists, err := rdb.Exists(context.Background(), "link:"+code).Result()
	if err != nil {
		t.Fatalf("redis exists: %v", err)
	}
	if exists != 0 {
		t.Fatal("an expired link must never be cached")
	}
	ev.add("redis link:%s exists = 0", code)
}

func TestDeleteInvalidatesCache(t *testing.T) {
	ev := newEvidence(t)
	code := "delete" + runTag

	createLink(t, mainKey, "https://example.com/delete", code)
	resp, body := resolve(t, code)
	mustStatus(t, resp, body, http.StatusFound)

	resp, body = request(t, http.MethodDelete, "/api/v1/links/"+code, mainKey, nil)
	mustStatus(t, resp, body, http.StatusNoContent)
	ev.add("DELETE api -> 204")

	resp, body = resolve(t, code)
	mustStatus(t, resp, body, http.StatusNotFound)
	ev.add("GET /%s -> 404 immediately after delete", code)

	exists, err := rdb.Exists(context.Background(), "link:"+code).Result()
	if err != nil {
		t.Fatalf("redis exists: %v", err)
	}
	if exists != 0 {
		t.Fatal("deleted link should be evicted from cache")
	}
	ev.add("redis link:%s exists = 0", code)
}

func TestDurabilityAcrossRestart(t *testing.T) {
	ev := newEvidence(t)
	code := "durable" + runTag
	target := "https://example.com/durable"

	createLink(t, mainKey, target, code)
	if err := rdb.Del(context.Background(), "link:"+code).Err(); err != nil {
		t.Fatalf("clear cache: %v", err)
	}
	ev.add("created %s and evicted its cache entry", code)

	restart(t)
	ev.add("api restarted on a new port %s", baseURL)

	resp, body := resolve(t, code)
	mustStatus(t, resp, body, http.StatusFound)
	if got := resp.Header.Get("Location"); got != target {
		t.Fatalf("after restart Location = %q, want %q", got, target)
	}
	ev.add("after restart GET /%s -> 302 %s (served by Postgres)", code, target)
}

func TestUnauthorized(t *testing.T) {
	ev := newEvidence(t)

	resp, body := request(t, http.MethodPost, "/api/v1/links", "", map[string]any{
		"target_url": "https://example.com/x",
	})
	mustStatus(t, resp, body, http.StatusUnauthorized)
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Fatal("401 should advertise the auth scheme")
	}
	ev.add("no key -> 401 with WWW-Authenticate")

	resp, body = request(t, http.MethodPost, "/api/v1/links", "not-a-real-key-at-all", map[string]any{
		"target_url": "https://example.com/x",
	})
	mustStatus(t, resp, body, http.StatusUnauthorized)
	ev.add("bad key -> 401")
}

func TestRateLimit(t *testing.T) {
	ev := newEvidence(t)

	// The limiter is per account, so this burns the second user's budget only.
	var last *http.Response
	allowed, limited := 0, 0
	for i := 0; i < 105; i++ {
		resp, body := request(t, http.MethodGet, "/api/v1/links?limit=1", limitKey, nil)
		if resp.StatusCode == http.StatusOK {
			allowed++
			last = resp
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			limited++
			if limited == 1 {
				if resp.Header.Get("Retry-After") == "" {
					t.Fatal("429 should include Retry-After")
				}
				ev.add("first 429 after %d allowed; Retry-After=%s Retry-Limit=%s",
					allowed, resp.Header.Get("Retry-After"), resp.Header.Get("X-RateLimit-Limit"))
			}
			_ = body
			continue
		}
		t.Fatalf("unexpected status %d", resp.StatusCode)
	}

	if allowed > 100 {
		t.Fatalf("allowed %d requests, limit is 100", allowed)
	}
	if limited == 0 {
		t.Fatal("expected at least one 429")
	}
	if last != nil && last.Header.Get("X-RateLimit-Limit") != "100" {
		t.Fatalf("X-RateLimit-Limit = %q, want 100", last.Header.Get("X-RateLimit-Limit"))
	}
	ev.add("sent 105 requests: %d allowed, %d limited", allowed, limited)
}

// --- report -------------------------------------------------------------

func writeReport() {
	resultsMu.Lock()
	defer resultsMu.Unlock()

	var b strings.Builder
	fmt.Fprintf(&b, "# E2E report\n\n")
	fmt.Fprintf(&b, "- Generated: %s\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(&b, "- API base URL: %s\n", baseURL)
	fmt.Fprintf(&b, "- Binary: `%s`\n", binPath)
	fmt.Fprintf(&b, "- Postgres: `%s`\n", databaseURL)
	fmt.Fprintf(&b, "- Redis: `%s`\n\n", redisAddr)

	passed, failed := 0, 0
	for _, r := range results {
		if r.Passed {
			passed++
		} else {
			failed++
		}
	}
	verdict := "PASS"
	if failed > 0 {
		verdict = "FAIL"
	}
	fmt.Fprintf(&b, "**%s - %d passed, %d failed**\n\n", verdict, passed, failed)

	b.WriteString("| scenario | result | duration |\n|---|---|---|\n")
	for _, r := range results {
		status := "pass"
		if !r.Passed {
			status = "**FAIL**"
		}
		fmt.Fprintf(&b, "| %s | %s | %s |\n", r.Name, status, r.Duration.Round(time.Millisecond))
	}

	for _, r := range results {
		fmt.Fprintf(&b, "\n### %s\n\n", r.Name)
		for _, line := range r.Evidence {
			fmt.Fprintf(&b, "- %s\n", line)
		}
	}

	md := []byte(b.String())
	if err := os.WriteFile(filepath.Join("..", "e2e-report.md"), md, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write markdown report: %v\n", err)
	}
	if raw, err := json.MarshalIndent(results, "", "  "); err == nil {
		if err := os.WriteFile(filepath.Join("..", "e2e-report.json"), raw, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "write json report: %v\n", err)
		}
	}
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func isBase62(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		default:
			return false
		}
	}
	return true
}
