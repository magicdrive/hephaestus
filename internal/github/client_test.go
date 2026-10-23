package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testToken = "ghp_SECRETTOKEN0123456789abcdef"

type sleepRecorder struct {
	mu     sync.Mutex
	sleeps []time.Duration
}

func (s *sleepRecorder) sleep(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sleeps = append(s.sleeps, d)
}

func (s *sleepRecorder) get() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.sleeps...)
}

func newTestClient(t *testing.T, srv *httptest.Server, mod ...func(*Options)) (*Client, *sleepRecorder) {
	t.Helper()
	rec := &sleepRecorder{}
	o := Options{Token: testToken, APIURL: srv.URL, UploadURL: srv.URL, HTTPClient: srv.Client(), Sleep: rec.sleep}
	for _, m := range mod {
		m(&o)
	}
	c, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return c, rec
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func TestHeaders(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		writeJSON(w, 200, map[string]any{"id": 1, "tag_name": "v1.0.0"})
	}))
	defer srv.Close()
	c, _ := newTestClient(t, srv)
	if _, err := c.GetReleaseByTag(context.Background(), "o", "r", "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"Accept":               "application/vnd.github+json",
		"X-Github-Api-Version": APIVersion,
		"Authorization":        "Bearer " + testToken,
		"User-Agent":           DefaultUserAgent,
	} {
		if got.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, got.Get(k), v)
		}
	}
}

func TestNewRejectsBadURLsWithoutEchoingThem(t *testing.T) {
	for _, u := range []string{"ftp://x", "https://user:" + testToken + "@api.example.com", "https://x/?a=b", "relative/path"} {
		_, err := New(Options{APIURL: u})
		if err == nil {
			t.Fatalf("%s: expected error", u)
		}
		if strings.Contains(err.Error(), testToken) {
			t.Fatalf("error leaks credentials: %v", err)
		}
	}
	if _, err := New(Options{Token: "a b"}); err == nil {
		t.Fatal("token with whitespace accepted")
	}
}

// TestNewRequiresHTTPSExceptLoopback is a regression test: the token used to
// be sent over plain http to any host.
func TestNewRequiresHTTPSExceptLoopback(t *testing.T) {
	for _, u := range []string{"http://api.example.com", "http://ghes.example/api/v3", "http://10.0.0.1:8080", "http://localhost.example.com"} {
		if _, err := New(Options{Token: testToken, APIURL: u}); err == nil || !strings.Contains(err.Error(), "https") {
			t.Errorf("API URL %s: err = %v, want https error", u, err)
		}
		if _, err := New(Options{Token: testToken, UploadURL: u}); err == nil || !strings.Contains(err.Error(), "https") {
			t.Errorf("upload URL %s: err = %v, want https error", u, err)
		}
	}
	for _, u := range []string{"http://127.0.0.1:1234", "http://[::1]:1234", "http://localhost:1234", "https://ghes.example/api/v3"} {
		if _, err := New(Options{Token: testToken, APIURL: u, UploadURL: u}); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
}

func TestDefaultUploadURL(t *testing.T) {
	for _, tc := range []struct{ api, want string }{
		{"", "https://uploads.github.com"},
		{"https://api.github.com", "https://uploads.github.com"},
		{"https://API.GitHub.com/", "https://uploads.github.com"},
		{"https://ghes.example.com/api/v3", "https://ghes.example.com/api/uploads"},
		{"https://ghes.example.com:8443/api/v3", "https://ghes.example.com:8443/api/uploads"},
		{"https://api.octocorp.ghe.com", "https://uploads.octocorp.ghe.com"},
		{"http://127.0.0.1:9/api/v3", "http://127.0.0.1:9/api/uploads"},
	} {
		c, err := New(Options{APIURL: tc.api})
		if err != nil {
			t.Fatalf("%s: %v", tc.api, err)
		}
		if got := c.upload.String(); got != tc.want {
			t.Errorf("API %q: upload URL %q, want %q", tc.api, got, tc.want)
		}
	}
}

// TestGHESUploadStaysOnHost is a regression test: with a GHES API URL and no
// upload URL, assets (and the token) used to go to uploads.github.com.
func TestGHESUploadStaysOnHost(t *testing.T) {
	var path, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		writeJSON(w, 201, Asset{ID: 1, Name: "a", Size: 1, State: "uploaded"})
	}))
	defer srv.Close()
	c, err := New(Options{Token: testToken, APIURL: srv.URL + "/api/v3", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.UploadReleaseAsset(context.Background(), "o", "r", 7, "a", strings.NewReader("x"), 1, "application/octet-stream"); err != nil {
		t.Fatal(err)
	}
	if path != "/api/uploads/repos/o/r/releases/7/assets" || auth != "Bearer "+testToken {
		t.Fatalf("upload went to %q (auth %q)", path, auth)
	}
}

func TestRetryOn429WithRetryAfter(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) <= 2 {
			w.Header().Set("Retry-After", "7")
			writeJSON(w, 429, map[string]any{"message": "slow down"})
			return
		}
		writeJSON(w, 200, []Release{})
	}))
	defer srv.Close()
	c, rec := newTestClient(t, srv)
	if _, err := c.ListReleases(context.Background(), "o", "r"); err != nil {
		t.Fatal(err)
	}
	if got := rec.get(); len(got) != 2 || got[0] != 7*time.Second || got[1] != 7*time.Second {
		t.Fatalf("sleeps = %v", got)
	}
}

func TestRetryOnSecondaryRateLimit403(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("method %s", r.Method)
		}
		if n.Add(1) == 1 {
			w.Header().Set("Retry-After", "3")
			writeJSON(w, 403, map[string]any{"message": "You have exceeded a secondary rate limit."})
			return
		}
		writeJSON(w, 200, Release{ID: 5, TagName: "v1"})
	}))
	defer srv.Close()
	c, rec := newTestClient(t, srv)
	if _, err := c.UpdateRelease(context.Background(), "o", "r", 5, ReleaseRequest{Draft: Bool(false)}); err != nil {
		t.Fatal(err)
	}
	if got := rec.get(); len(got) != 1 || got[0] != 3*time.Second {
		t.Fatalf("sleeps = %v", got)
	}
}

func TestSecondaryRateLimitWithoutHeadersWaitsAMinute(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			writeJSON(w, 403, map[string]any{"message": "You have exceeded a secondary rate limit"})
			return
		}
		w.WriteHeader(204)
	}))
	defer srv.Close()
	c, rec := newTestClient(t, srv)
	if err := c.DeleteReleaseAsset(context.Background(), "o", "r", 1); err != nil {
		t.Fatal(err)
	}
	if got := rec.get(); len(got) != 1 || got[0] < time.Minute {
		t.Fatalf("sleeps = %v", got)
	}
}

func TestRetryUsesRateLimitReset(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(now.Unix()+10, 10))
			writeJSON(w, 403, map[string]any{"message": "API rate limit exceeded"})
			return
		}
		writeJSON(w, 200, Repository{DefaultBranch: "main"})
	}))
	defer srv.Close()
	c, rec := newTestClient(t, srv, func(o *Options) { o.Now = func() time.Time { return now } })
	r, err := c.GetRepository(context.Background(), "o", "r")
	if err != nil {
		t.Fatal(err)
	}
	if r.DefaultBranch != "main" {
		t.Fatal(r.DefaultBranch)
	}
	if got := rec.get(); len(got) != 1 || got[0] != 11*time.Second {
		t.Fatalf("sleeps = %v", got)
	}
}

func TestNoRetryOnPermission403(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		writeJSON(w, 403, map[string]any{"message": "Resource not accessible by integration"})
	}))
	defer srv.Close()
	c, _ := newTestClient(t, srv)
	_, err := c.GetRepository(context.Background(), "o", "r")
	if StatusCode(err) != 403 || n.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, n.Load())
	}
}

func TestRetry5xxWithBackoffAndLimit(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		writeJSON(w, 502, map[string]any{"message": "Bad Gateway"})
	}))
	defer srv.Close()
	c, rec := newTestClient(t, srv, func(o *Options) { o.MaxRetries = 3 })
	_, err := c.GetRepository(context.Background(), "o", "r")
	if StatusCode(err) != 502 {
		t.Fatalf("err = %v", err)
	}
	if n.Load() != 4 {
		t.Fatalf("calls = %d, want 4", n.Load())
	}
	got := rec.get()
	if len(got) != 3 {
		t.Fatalf("sleeps = %v", got)
	}
	for i, d := range got {
		base := time.Second << i
		if d < base/2 || d > base {
			t.Errorf("sleep %d = %s, want within [%s, %s]", i, d, base/2, base)
		}
	}
}

func TestRetryWaitTooLongFails(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.Header().Set("Retry-After", "3600")
		writeJSON(w, 429, map[string]any{"message": "later"})
	}))
	defer srv.Close()
	c, rec := newTestClient(t, srv)
	_, err := c.GetRepository(context.Background(), "o", "r")
	if err == nil || n.Load() != 1 || len(rec.get()) != 0 {
		t.Fatalf("err=%v calls=%d sleeps=%v", err, n.Load(), rec.get())
	}
}

func TestPOSTIsNeverRetried(t *testing.T) {
	for _, status := range []int{429, 500, 502, 503} {
		var n atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Retry-After", "1")
			writeJSON(w, status, map[string]any{"message": "nope"})
		}))
		c, rec := newTestClient(t, srv)
		ctx := context.Background()
		calls := []func() error{
			func() error { _, err := c.CreateRelease(ctx, "o", "r", ReleaseRequest{TagName: "v1"}); return err },
			func() error {
				_, err := c.UploadReleaseAsset(ctx, "o", "r", 1, "a.tar.gz", strings.NewReader("abc"), 3, "")
				return err
			},
			func() error { _, err := c.CreateRef(ctx, "o", "r", "heads/x", "abc"); return err },
			func() error {
				_, err := c.CreatePull(ctx, "o", "r", NewPullRequest{Title: "t", Head: "o:x", Base: "main"})
				return err
			},
		}
		for i, f := range calls {
			n.Store(0)
			if err := f(); StatusCode(err) != status {
				t.Fatalf("status %d call %d: err = %v", status, i, err)
			}
			if n.Load() != 1 {
				t.Fatalf("status %d call %d: %d requests, want 1", status, i, n.Load())
			}
		}
		if len(rec.get()) != 0 {
			t.Fatalf("POST slept: %v", rec.get())
		}
		// RetryDelay still tells the caller it was transient.
		if _, ok := c.RetryDelay(calls[0](), 0); !ok {
			t.Fatalf("status %d: RetryDelay should report transient", status)
		}
		srv.Close()
	}
}

func TestTransportErrorRetriedForGETOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // connection refused from now on
	rec := &sleepRecorder{}
	c, err := New(Options{Token: testToken, APIURL: url, UploadURL: url, Sleep: rec.sleep, MaxRetries: 2})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.GetRepository(context.Background(), "o", "r")
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("err = %T %v", err, err)
	}
	if len(rec.get()) != 2 {
		t.Fatalf("GET sleeps = %v", rec.get())
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatal("token in transport error")
	}
	rec.sleeps = nil
	_, err = c.CreateRelease(context.Background(), "o", "r", ReleaseRequest{TagName: "v1"})
	if !errors.As(err, &te) || len(rec.get()) != 0 {
		t.Fatalf("POST err=%v sleeps=%v", err, rec.get())
	}
}

func TestTokenNeverInErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A hostile or buggy server echoing the credential back.
		writeJSON(w, 401, map[string]any{
			"message":           "Bad credentials: " + r.Header.Get("Authorization"),
			"errors":            []any{"token " + testToken, map[string]any{"code": "invalid", "message": testToken}},
			"documentation_url": "https://docs.github.com/?t=" + testToken,
		})
	}))
	defer srv.Close()
	c, _ := newTestClient(t, srv)
	_, err := c.GetRepository(context.Background(), "o", "r")
	if err == nil {
		t.Fatal("expected error")
	}
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("%T", err)
	}
	all := fmt.Sprintf("%v %+v %#v", err, ae, ae)
	if strings.Contains(all, testToken) {
		t.Fatalf("token leaked: %s", all)
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("unhelpful error: %v", err)
	}
}

func TestRedirectToOtherHostDropsAuthorization(t *testing.T) {
	var storageAuth atomic.Value
	storageAuth.Store("unset")
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		storageAuth.Store(r.Header.Get("Authorization"))
		_, _ = w.Write([]byte("payload"))
	}))
	defer storage.Close()
	var apiAuth []string
	var mu sync.Mutex
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		apiAuth = append(apiAuth, r.Header.Get("Authorization"))
		mu.Unlock()
		switch r.URL.Path {
		case "/repos/o/r/releases/assets/1":
			// Same-host hop first, then to object storage.
			http.Redirect(w, r, "/hop", http.StatusFound)
		case "/hop":
			if r.Header.Get("Accept") != "application/octet-stream" {
				t.Errorf("accept = %q", r.Header.Get("Accept"))
			}
			http.Redirect(w, r, storage.URL+"/blob", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	c, _ := newTestClient(t, api)
	var buf bytes.Buffer
	n, err := c.DownloadReleaseAsset(context.Background(), "o", "r", 1, &buf, 100)
	if err != nil {
		t.Fatal(err)
	}
	if n != 7 || buf.String() != "payload" {
		t.Fatalf("got %d %q", n, buf.String())
	}
	if got := storageAuth.Load().(string); got != "" {
		t.Fatalf("Authorization sent to other host: %q", got)
	}
	for _, a := range apiAuth {
		if a != "Bearer "+testToken {
			t.Fatalf("same-host request lost Authorization: %q", a)
		}
	}
}

func TestRedirectHTTPSToHTTPRefused(t *testing.T) {
	var hit atomic.Bool
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit.Store(true) }))
	defer plain.Close()
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/x", http.StatusFound)
	}))
	defer tlsSrv.Close()
	c, _ := newTestClient(t, tlsSrv)
	_, err := c.DownloadReleaseAsset(context.Background(), "o", "r", 1, io.Discard, 100)
	if err == nil || !strings.Contains(err.Error(), "non-https") {
		t.Fatalf("err = %v", err)
	}
	if hit.Load() {
		t.Fatal("followed downgrade redirect")
	}
}

func TestDownloadSizeLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), 1000))
	}))
	defer srv.Close()
	c, rec := newTestClient(t, srv)
	_, err := c.DownloadReleaseAsset(context.Background(), "o", "r", 1, io.Discard, 999)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("err = %v", err)
	}
	if len(rec.get()) != 0 {
		t.Fatal("size limit error must not be retried")
	}
}

func TestPaginationFollowsLinkWithinOrigin(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("per_page") != "100" {
			t.Errorf("per_page = %q", r.URL.Query().Get("per_page"))
		}
		switch r.URL.Query().Get("page") {
		case "":
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/releases?per_page=100&page=2>; rel="next", <%s/x>; rel="last"`, srv.URL, srv.URL))
			writeJSON(w, 200, []Release{{ID: 1, TagName: "v2"}})
		case "2":
			writeJSON(w, 200, []Release{{ID: 2, TagName: "v1"}})
		}
	}))
	defer srv.Close()
	c, _ := newTestClient(t, srv)
	got, err := c.ListReleases(context.Background(), "o", "r")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].ID != 2 {
		t.Fatalf("got %+v", got)
	}

	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("followed foreign pagination link")
	}))
	defer evil.Close()
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", fmt.Sprintf(`<%s/steal>; rel="next"`, evil.URL))
		writeJSON(w, 200, []Release{})
	}))
	defer srv2.Close()
	c2, _ := newTestClient(t, srv2)
	if _, err := c2.ListReleases(context.Background(), "o", "r"); err == nil {
		t.Fatal("expected error for foreign link")
	}
}

func TestFindReleaseByTagPrefersPublishedThenOldestDraft(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, []Release{
			{ID: 9, TagName: "v1", Draft: true},
			{ID: 3, TagName: "v1", Draft: true},
			{ID: 4, TagName: "v2", Draft: true},
			{ID: 7, TagName: "v2"},
		})
	}))
	defer srv.Close()
	c, _ := newTestClient(t, srv)
	ctx := context.Background()
	if r, err := c.FindReleaseByTag(ctx, "o", "r", "v1"); err != nil || r.ID != 3 {
		t.Fatalf("v1: %+v %v", r, err)
	}
	if r, err := c.FindReleaseByTag(ctx, "o", "r", "v2"); err != nil || r.ID != 7 {
		t.Fatalf("v2: %+v %v", r, err)
	}
	if _, err := c.FindReleaseByTag(ctx, "o", "r", "v3"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("v3: %v", err)
	}
}

func TestAPIErrorParsing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/releases/tags/v1":
			writeJSON(w, 404, map[string]any{"message": "Not Found"})
		default:
			writeJSON(w, 422, map[string]any{
				"message": "Validation Failed",
				"errors":  []any{map[string]any{"resource": "Release", "field": "tag_name", "code": "already_exists"}, "plain string"},
			})
		}
	}))
	defer srv.Close()
	c, _ := newTestClient(t, srv)
	ctx := context.Background()
	_, err := c.GetReleaseByTag(ctx, "o", "r", "v1")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("not ErrNotFound: %v", err)
	}
	_, err = c.CreateRelease(ctx, "o", "r", ReleaseRequest{TagName: "v1"})
	if !IsAlreadyExists(err) {
		t.Fatalf("not already_exists: %v", err)
	}
	if !strings.Contains(err.Error(), "Release.tag_name already_exists") || !strings.Contains(err.Error(), "plain string") {
		t.Fatalf("error text: %v", err)
	}
}

func TestUploadSendsContentLengthAndName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/repos/o/r/releases/42/assets" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		if r.ContentLength != 5 || len(r.TransferEncoding) != 0 {
			t.Errorf("content-length %d, te %v", r.ContentLength, r.TransferEncoding)
		}
		if r.Header.Get("Content-Type") != "application/gzip" {
			t.Errorf("content-type %q", r.Header.Get("Content-Type"))
		}
		b, _ := io.ReadAll(r.Body)
		d := "sha256:abc"
		writeJSON(w, 201, Asset{ID: 1, Name: r.URL.Query().Get("name"), Size: int64(len(b)), State: "uploaded", Digest: &d})
	}))
	defer srv.Close()
	c, _ := newTestClient(t, srv)
	// A reader whose length the transport cannot infer.
	a, err := c.UploadReleaseAsset(context.Background(), "o", "r", 42, "x_1.0.tar.gz", io.MultiReader(strings.NewReader("hel"), strings.NewReader("lo")), 5, "application/gzip")
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "x_1.0.tar.gz" || a.Size != 5 || *a.Digest != "sha256:abc" {
		t.Fatalf("%+v", a)
	}
}

func TestWaitHonoursContext(t *testing.T) {
	c, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	start := time.Now()
	if err := c.Wait(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("Wait ignored cancellation")
	}
}

func TestRequestTimeout(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(block)
	c, _ := newTestClient(t, srv, func(o *Options) { o.RequestTimeout = 50 * time.Millisecond; o.MaxRetries = -1 })
	_, err := c.GetRepository(context.Background(), "o", "r")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func TestContentsRefsPulls(t *testing.T) {
	formula := []byte("class Foo < Formula\nend\n")
	enc := base64.StdEncoding.EncodeToString(formula)
	wrapped := enc[:10] + "\n" + enc[10:] + "\n"
	var putBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.EscapedPath() == "/repos/o/tap/contents/Formula/foo.rb":
			if r.URL.Query().Get("ref") != "main" {
				t.Errorf("ref = %q", r.URL.Query().Get("ref"))
			}
			writeJSON(w, 200, map[string]any{"type": "file", "path": "Formula/foo.rb", "sha": "blob1", "content": wrapped, "encoding": "base64", "size": len(formula)})
		case r.Method == "GET" && r.URL.Path == "/repos/o/tap/contents/Formula/missing.rb":
			writeJSON(w, 404, map[string]any{"message": "Not Found"})
		case r.Method == "GET" && r.URL.Path == "/repos/o/tap/contents/Formula":
			writeJSON(w, 200, []any{})
		case r.Method == "PUT" && r.URL.Path == "/repos/o/tap/contents/Formula/foo.rb":
			_ = json.NewDecoder(r.Body).Decode(&putBody)
			writeJSON(w, 200, map[string]any{"content": map[string]any{"sha": "blob2"}, "commit": map[string]any{"sha": "c2", "html_url": "https://x/c2"}})
		case r.Method == "GET" && r.URL.EscapedPath() == "/repos/o/tap/git/ref/heads/feature/x":
			writeJSON(w, 200, map[string]any{"ref": "refs/heads/feature/x", "object": map[string]any{"sha": "s1", "type": "commit"}})
		case r.Method == "POST" && r.URL.Path == "/repos/o/tap/git/refs":
			var b map[string]string
			_ = json.NewDecoder(r.Body).Decode(&b)
			if b["ref"] != "refs/heads/bump" || b["sha"] != "s1" {
				t.Errorf("create ref body %v", b)
			}
			writeJSON(w, 201, map[string]any{"ref": b["ref"], "object": map[string]any{"sha": b["sha"]}})
		case r.Method == "GET" && r.URL.Path == "/repos/o/tap":
			writeJSON(w, 200, map[string]any{"default_branch": "trunk", "full_name": "o/tap"})
		case r.Method == "GET" && r.URL.Path == "/repos/o/tap/pulls":
			q := r.URL.Query()
			if q.Get("head") != "o:bump" || q.Get("state") != "open" {
				t.Errorf("pulls query %v", q)
			}
			writeJSON(w, 200, []any{map[string]any{"number": 3, "html_url": "https://x/pull/3"}})
		case r.Method == "POST" && r.URL.Path == "/repos/o/tap/pulls":
			var b NewPullRequest
			_ = json.NewDecoder(r.Body).Decode(&b)
			writeJSON(w, 201, map[string]any{"number": 4, "title": b.Title, "html_url": "https://x/pull/4"})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.EscapedPath())
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	c, _ := newTestClient(t, srv)
	ctx := context.Background()

	fc, err := c.GetContents(ctx, "o", "tap", "Formula/foo.rb", "main")
	if err != nil {
		t.Fatal(err)
	}
	if fc.SHA != "blob1" || !bytes.Equal(fc.Content, formula) {
		t.Fatalf("%+v", fc)
	}
	if _, err := c.GetContents(ctx, "o", "tap", "Formula/missing.rb", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := c.GetContents(ctx, "o", "tap", "Formula", ""); err == nil {
		t.Fatal("directory accepted")
	}
	if _, err := c.GetContents(ctx, "o", "tap", "../x", ""); err == nil {
		t.Fatal("dot-dot path accepted")
	}

	res, err := c.PutContents(ctx, "o", "tap", "Formula/foo.rb", PutContentsRequest{
		Message: "foo 1.0", Content: []byte("new"), SHA: "blob1", Branch: "bump",
		Author: &CommitAuthor{Name: "A", Email: "a@x"}, Committer: &CommitAuthor{Name: "C", Email: "c@x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ContentSHA != "blob2" || res.CommitSHA != "c2" {
		t.Fatalf("%+v", res)
	}
	if putBody["sha"] != "blob1" || putBody["branch"] != "bump" || putBody["content"] != base64.StdEncoding.EncodeToString([]byte("new")) {
		t.Fatalf("put body %v", putBody)
	}
	if a, _ := putBody["author"].(map[string]any); a["email"] != "a@x" {
		t.Fatalf("author %v", putBody["author"])
	}
	if _, err := c.PutContents(ctx, "o", "tap", "f", PutContentsRequest{Message: "m", Author: &CommitAuthor{Name: "x"}}); err == nil {
		t.Fatal("author without email accepted")
	}

	ref, err := c.GetRef(ctx, "o", "tap", "refs/heads/feature/x")
	if err != nil || ref.Object.SHA != "s1" {
		t.Fatalf("%+v %v", ref, err)
	}
	if _, err := c.CreateRef(ctx, "o", "tap", "heads/bump", "s1"); err != nil {
		t.Fatal(err)
	}
	repo, err := c.GetRepository(ctx, "o", "tap")
	if err != nil || repo.DefaultBranch != "trunk" {
		t.Fatalf("%+v %v", repo, err)
	}
	prs, err := c.ListPulls(ctx, "o", "tap", ListPullsOptions{Head: "o:bump"})
	if err != nil || len(prs) != 1 || prs[0].Number != 3 {
		t.Fatalf("%+v %v", prs, err)
	}
	if _, err := c.ListPulls(ctx, "o", "tap", ListPullsOptions{Head: "bump"}); err == nil {
		t.Fatal("head without owner accepted")
	}
	pr, err := c.CreatePull(ctx, "o", "tap", NewPullRequest{Title: "foo 1.0", Head: "bump", Base: "trunk"})
	if err != nil || pr.Number != 4 || pr.Title != "foo 1.0" {
		t.Fatalf("%+v %v", pr, err)
	}
}

func TestNextLink(t *testing.T) {
	cases := map[string]string{
		`<https://a/x?page=2>; rel="next", <https://a/x?page=5>; rel="last"`: "https://a/x?page=2",
		`<https://a/x?page=1>; rel="prev"`:                                   "",
		`<https://a/x?page=3>; rel="last next"`:                              "https://a/x?page=3",
		``:                                                                   "",
	}
	for in, want := range cases {
		if got := nextLink(in); got != want {
			t.Errorf("nextLink(%q) = %q, want %q", in, got, want)
		}
	}
}
