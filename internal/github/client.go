// Package github is a small GitHub REST API client built on net/http only.
//
// It covers the endpoints Hephaestus needs to publish releases and Homebrew
// taps. Its safety rules:
//
//   - The token is sent only to the origin (scheme and host) of the request
//     being made. A redirect to another origin is followed without the
//     Authorization header (asset downloads redirect to object storage), and a
//     redirect from https to http is refused.
//   - Pagination links are followed only within the API origin.
//   - Requests with idempotent methods (GET, PUT, PATCH, DELETE) are retried
//     on 5xx, 429, secondary-rate-limit 403 and network errors. POST is never
//     retried automatically: creating a release, uploading an asset, creating
//     a ref or a pull request may have succeeded even when the response was
//     lost, so callers re-read state before trying again (see RetryDelay).
//   - Response bodies are size-limited, every attempt has a timeout, and the
//     token never appears in an error message.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Defaults for Options.
const (
	DefaultAPIURL          = "https://api.github.com"
	DefaultUploadURL       = "https://uploads.github.com"
	DefaultUserAgent       = "hephaestus"
	DefaultMaxRetries      = 4
	DefaultRequestTimeout  = 60 * time.Second
	DefaultTransferTimeout = 30 * time.Minute
	DefaultMaxRetryWait    = 15 * time.Minute

	// APIVersion is sent as X-GitHub-Api-Version.
	APIVersion = "2022-11-28"

	// MaxResponseSize bounds a JSON response body (one page of a listing).
	MaxResponseSize = 32 << 20

	maxErrorBody  = 1 << 20
	maxRedirects  = 10
	maxPages      = 1000
	perPage       = 100
	maxBackoff    = 30 * time.Second
	secondaryWait = time.Minute
)

// ErrNotFound matches (via errors.Is) an *APIError with status 404 and the
// "not found" result of lookups such as FindReleaseByTag.
var ErrNotFound = errors.New("github: not found")

// Options configures a Client.
type Options struct {
	// Token is sent as "Authorization: Bearer <token>". Empty sends no
	// Authorization header.
	Token string
	// APIURL defaults to https://api.github.com. For GitHub Enterprise
	// Server use https://HOST/api/v3.
	APIURL string
	// UploadURL defaults to https://uploads.github.com when APIURL is
	// github.com's, to https://uploads.SUB.ghe.com for GHE.com
	// (https://api.SUB.ghe.com), and otherwise to https://HOST/api/uploads
	// on the API URL's host (GitHub Enterprise Server), so a GHES token is
	// never sent to github.com. Assets are always uploaded here, never to a
	// release's upload_url, so the token only goes to configured hosts.
	//
	// Both URLs must use https; plain http is accepted only for a loopback
	// host (127.0.0.1, ::1, localhost), for local test servers.
	UploadURL string
	// HTTPClient is copied; its CheckRedirect is replaced because redirects
	// are handled by this package.
	HTTPClient *http.Client
	UserAgent  string
	// Sleep replaces the wait between retries (tests). When nil, waits use a
	// timer and end early when the context is done.
	Sleep func(time.Duration)
	// MaxRetries is the number of retries after the first attempt of an
	// idempotent request. 0 selects DefaultMaxRetries; negative disables
	// retries.
	MaxRetries int
	// RequestTimeout bounds one attempt of an API call (default 60s).
	RequestTimeout time.Duration
	// TransferTimeout bounds one asset upload or download (default 30m).
	TransferTimeout time.Duration
	// MaxRetryWait is the longest single wait accepted from Retry-After or
	// x-ratelimit-reset; a longer demand fails the request instead
	// (default 15m).
	MaxRetryWait time.Duration
	// Now replaces time.Now (tests).
	Now func() time.Time
	// Logf, when set, receives retry notices. Messages never contain the
	// token.
	Logf func(format string, args ...any)
}

// Client is a GitHub REST API client. It is safe for concurrent use.
type Client struct {
	token       string
	api, upload *url.URL
	hc          *http.Client
	ua          string
	sleep       func(time.Duration)
	maxRetries  int
	reqTimeout  time.Duration
	xferTimeout time.Duration
	maxWait     time.Duration
	now         func() time.Time
	logf        func(string, ...any)
}

// New returns a client for o.
func New(o Options) (*Client, error) {
	api, err := parseBase(o.APIURL, DefaultAPIURL, "API URL")
	if err != nil {
		return nil, err
	}
	upload, err := parseBase(o.UploadURL, defaultUploadURL(api), "upload URL")
	if err != nil {
		return nil, err
	}
	if strings.ContainsAny(o.Token, "\r\n\t ") {
		return nil, errors.New("github: token contains whitespace")
	}
	var hc http.Client
	if o.HTTPClient != nil {
		hc = *o.HTTPClient
	} else {
		hc.Transport = defaultTransport()
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c := &Client{
		token: o.Token, api: api, upload: upload, hc: &hc,
		ua: o.UserAgent, sleep: o.Sleep, maxRetries: o.MaxRetries,
		reqTimeout: o.RequestTimeout, xferTimeout: o.TransferTimeout, maxWait: o.MaxRetryWait,
		now: o.Now, logf: o.Logf,
	}
	if c.ua == "" {
		c.ua = DefaultUserAgent
	}
	switch {
	case c.maxRetries == 0:
		c.maxRetries = DefaultMaxRetries
	case c.maxRetries < 0:
		c.maxRetries = 0
	}
	if c.reqTimeout <= 0 {
		c.reqTimeout = DefaultRequestTimeout
	}
	if c.xferTimeout <= 0 {
		c.xferTimeout = DefaultTransferTimeout
	}
	if c.maxWait <= 0 {
		c.maxWait = DefaultMaxRetryWait
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.logf == nil {
		c.logf = func(string, ...any) {}
	}
	return c, nil
}

func defaultTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	t.TLSHandshakeTimeout = 15 * time.Second
	t.ResponseHeaderTimeout = 5 * time.Minute
	t.ExpectContinueTimeout = time.Second
	return t
}

func parseBase(s, def, what string) (*url.URL, error) {
	if s == "" {
		s = def
	}
	u, err := url.Parse(strings.TrimRight(s, "/"))
	// The value is not echoed: it could carry credentials.
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, fmt.Errorf("github: invalid %s: must be an absolute https URL without credentials, query or fragment", what)
	}
	// The token is sent to this URL: plain http would expose it on the
	// network. Local test servers are the only exception.
	if u.Scheme == "http" && !isLoopback(u.Hostname()) {
		return nil, fmt.Errorf("github: invalid %s: must use https (http is accepted only for a loopback host)", what)
	}
	return u, nil
}

// isLoopback reports whether host is localhost or a loopback IP address.
func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// defaultUploadURL derives the upload URL from the API URL: github.com's
// uploads host for api.github.com, uploads.SUB.ghe.com for GHE.com, and
// SCHEME://HOST/api/uploads for GitHub Enterprise Server.
func defaultUploadURL(api *url.URL) string {
	host := strings.ToLower(api.Hostname())
	switch {
	case host == "api.github.com":
		return DefaultUploadURL
	case strings.HasPrefix(host, "api.") && strings.HasSuffix(host, ".ghe.com"):
		return api.Scheme + "://uploads." + strings.TrimPrefix(strings.ToLower(api.Host), "api.")
	default:
		return (&url.URL{Scheme: api.Scheme, Host: api.Host, Path: "/api/uploads"}).String()
	}
}

// String returns a pointer to s, for optional request fields.
func String(s string) *string { return &s }

// Bool returns a pointer to b, for optional request fields.
func Bool(b bool) *bool { return &b }

// ErrorDetail is one entry of an API error's "errors" list.
type ErrorDetail struct {
	Resource string `json:"resource,omitempty"`
	Field    string `json:"field,omitempty"`
	Code     string `json:"code,omitempty"`
	Message  string `json:"message,omitempty"`
}

// UnmarshalJSON accepts the object form and the plain string form GitHub
// uses for some endpoints.
func (d *ErrorDetail) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*d = ErrorDetail{Message: s}
		return nil
	}
	type plain ErrorDetail
	return json.Unmarshal(b, (*plain)(d))
}

// APIError is a non-2xx response.
type APIError struct {
	StatusCode       int
	Message          string
	Errors           []ErrorDetail
	DocumentationURL string
	Method           string
	URL              string

	header http.Header
}

func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "github: %s %s: %d", e.Method, e.URL, e.StatusCode)
	if e.Message != "" {
		b.WriteString(" " + e.Message)
	} else if t := http.StatusText(e.StatusCode); t != "" {
		b.WriteString(" " + t)
	}
	for _, d := range e.Errors {
		var parts []string
		if d.Resource != "" || d.Field != "" {
			parts = append(parts, strings.Trim(d.Resource+"."+d.Field, "."))
		}
		for _, s := range []string{d.Code, d.Message} {
			if s != "" {
				parts = append(parts, s)
			}
		}
		if len(parts) > 0 {
			b.WriteString(" [" + strings.Join(parts, " ") + "]")
		}
	}
	return b.String()
}

// Is makes errors.Is(err, ErrNotFound) true for 404 responses.
func (e *APIError) Is(target error) bool {
	return target == ErrNotFound && e.StatusCode == http.StatusNotFound
}

// HasCode reports whether any error detail carries code (e.g.
// "already_exists").
func (e *APIError) HasCode(code string) bool {
	for _, d := range e.Errors {
		if d.Code == code {
			return true
		}
	}
	return false
}

// RateLimited reports whether the response is a primary or secondary rate
// limit (429, or 403 with rate-limit headers or message).
func (e *APIError) RateLimited() bool {
	if e.StatusCode == http.StatusTooManyRequests {
		return true
	}
	if e.StatusCode != http.StatusForbidden {
		return false
	}
	return e.header.Get("Retry-After") != "" || e.header.Get("X-RateLimit-Remaining") == "0" ||
		strings.Contains(strings.ToLower(e.Message), "rate limit")
}

// IsAlreadyExists reports whether err is a 422 whose details contain
// code "already_exists".
func IsAlreadyExists(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.StatusCode == http.StatusUnprocessableEntity && ae.HasCode("already_exists")
}

// StatusCode returns the HTTP status of an *APIError in err's chain, or 0.
func StatusCode(err error) int {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.StatusCode
	}
	return 0
}

// TransportError is a failure to obtain a response (connection error,
// timeout). The request may or may not have reached GitHub.
type TransportError struct {
	Method, URL string
	msg         string
	err         error
}

func (e *TransportError) Error() string {
	return fmt.Sprintf("github: %s %s: %s", e.Method, e.URL, e.msg)
}

// Unwrap returns the underlying error (e.g. context.DeadlineExceeded).
func (e *TransportError) Unwrap() error { return e.err }

// RetryDelay reports whether err is transient and how long to wait before
// attempt number attempt+1 (attempt counts from 0). It honours Retry-After
// and x-ratelimit-reset, and otherwise uses exponential backoff with jitter.
// Callers retrying a POST must re-read state before resending.
func (c *Client) RetryDelay(err error, attempt int) (time.Duration, bool) {
	var ae *APIError
	if errors.As(err, &ae) {
		switch {
		case ae.RateLimited():
			if d, ok := c.serverDelay(ae.header); ok {
				return d, true
			}
			return max(secondaryWait, backoff(attempt)), true
		case ae.StatusCode >= 500:
			if d, ok := c.serverDelay(ae.header); ok {
				return d, true
			}
			return backoff(attempt), true
		}
		return 0, false
	}
	var te *TransportError
	if errors.As(err, &te) && !errors.Is(err, context.Canceled) {
		return backoff(attempt), true
	}
	return 0, false
}

func (c *Client) serverDelay(h http.Header) (time.Duration, bool) {
	if v := strings.TrimSpace(h.Get("Retry-After")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			return time.Duration(n) * time.Second, true
		}
		if t, err := http.ParseTime(v); err == nil {
			return max(0, t.Sub(c.now())), true
		}
	}
	if h.Get("X-RateLimit-Remaining") == "0" {
		if n, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			return max(0, time.Unix(n, 0).Sub(c.now())) + time.Second, true
		}
	}
	return 0, false
}

func backoff(attempt int) time.Duration {
	d := maxBackoff
	if attempt < 6 {
		d = min(maxBackoff, time.Second<<attempt)
	}
	return d/2 + rand.N(d/2+1) //nolint:gosec // G404: retry jitter only; not security-sensitive
}

// Wait blocks for d, or until ctx is done. It uses Options.Sleep when set.
func (c *Client) Wait(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.sleep != nil {
		c.sleep(d)
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// call describes one logical request.
type call struct {
	method string
	url    string
	// body is a replayable JSON body.
	body []byte
	// stream is a non-replayable upload body of exactly size bytes.
	stream      io.Reader
	size        int64
	contentType string
	accept      string
	timeout     time.Duration
	// handle consumes a 2xx response; nil discards it.
	handle func(*http.Response) error
}

func (c *Client) redact(s string) string {
	if c.token == "" {
		return s
	}
	return strings.ReplaceAll(s, c.token, "[REDACTED]")
}

// do runs cl with retries and returns the final response headers.
func (c *Client) do(ctx context.Context, cl *call) (http.Header, error) {
	retryable := cl.stream == nil && idempotent(cl.method)
	for attempt := 0; ; attempt++ {
		h, err := c.once(ctx, cl)
		if err == nil {
			return h, nil
		}
		if ctx.Err() != nil || !retryable || attempt >= c.maxRetries {
			return nil, err
		}
		wait, ok := c.RetryDelay(err, attempt)
		if !ok {
			return nil, err
		}
		if wait > c.maxWait {
			return nil, fmt.Errorf("%w (retry would require waiting %s, more than %s)", err, wait.Round(time.Second), c.maxWait)
		}
		c.logf("github: %s %s failed (%d); retrying in %s (%d/%d)", cl.method, cl.url, StatusCode(err), wait.Round(time.Millisecond), attempt+1, c.maxRetries)
		if werr := c.Wait(ctx, wait); werr != nil {
			return nil, err
		}
	}
}

func idempotent(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

// once performs one attempt, following redirects.
func (c *Client) once(ctx context.Context, cl *call) (http.Header, error) {
	timeout := cl.timeout
	if timeout <= 0 {
		timeout = c.reqTimeout
	}
	actx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	origin, err := url.Parse(cl.url)
	if err != nil {
		return nil, fmt.Errorf("github: invalid request URL: %w", err)
	}
	cur, method, body, auth := origin, cl.method, cl.body, true
	for hop := 0; ; hop++ {
		req, err := c.newRequest(actx, method, cur, cl, body, hop == 0, auth)
		if err != nil {
			return nil, err
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return nil, cerr
			}
			return nil, &TransportError{Method: cl.method, URL: cl.url, msg: c.redact(err.Error()), err: err}
		}
		switch resp.StatusCode {
		case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
			loc := resp.Header.Get("Location")
			drain(resp)
			if loc == "" {
				return nil, &APIError{StatusCode: resp.StatusCode, Message: "redirect without Location", Method: cl.method, URL: cl.url, header: resp.Header}
			}
			if hop >= maxRedirects {
				return nil, fmt.Errorf("github: %s %s: too many redirects", cl.method, cl.url)
			}
			next, err := cur.Parse(loc)
			if err != nil {
				return nil, fmt.Errorf("github: %s %s: invalid redirect location", cl.method, cl.url)
			}
			if next.Scheme != "https" && !(next.Scheme == "http" && cur.Scheme == "http") {
				return nil, fmt.Errorf("github: %s %s: refusing redirect to non-https URL", cl.method, cl.url)
			}
			if !sameOrigin(next, origin) {
				// Never send the token to another origin, for the rest of
				// the chain.
				auth = false
			}
			readOnly := method == http.MethodGet || method == http.MethodHead
			switch {
			case resp.StatusCode == http.StatusSeeOther:
				if method != http.MethodHead {
					method = http.MethodGet
				}
				body = nil
			case readOnly:
			case resp.StatusCode == http.StatusTemporaryRedirect || resp.StatusCode == http.StatusPermanentRedirect:
				if cl.stream != nil || !sameOrigin(next, origin) {
					return nil, fmt.Errorf("github: %s %s: cannot follow redirect with a request body", cl.method, cl.url)
				}
			default:
				return nil, fmt.Errorf("github: %s %s: unexpected redirect (%d)", cl.method, cl.url, resp.StatusCode)
			}
			cur = next
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			ae := c.apiError(cl, resp)
			drain(resp)
			return resp.Header, ae
		}
		var herr error
		if cl.handle != nil {
			herr = cl.handle(resp)
		}
		drain(resp)
		if herr != nil {
			if cerr := ctx.Err(); cerr != nil {
				return nil, cerr
			}
			return nil, herr
		}
		return resp.Header, nil
	}
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close() // nothing actionable on close failure after draining
}

func (c *Client) newRequest(ctx context.Context, method string, u *url.URL, cl *call, body []byte, first, auth bool) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("github: %s: %w", method, err)
	}
	switch {
	case cl.stream != nil && first && method == cl.method:
		req.ContentLength = cl.size
		if cl.size == 0 {
			req.Body = http.NoBody
		} else {
			req.Body = io.NopCloser(cl.stream)
		}
		req.Header.Set("Content-Type", cl.contentType)
	case body != nil:
		req.ContentLength = int64(len(body))
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
		req.Header.Set("Content-Type", "application/json")
	}
	accept := cl.accept
	if accept == "" {
		accept = "application/vnd.github+json"
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("X-GitHub-Api-Version", APIVersion)
	if auth && c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	return req, nil
}

func (c *Client) apiError(cl *call, resp *http.Response) *APIError {
	ae := &APIError{StatusCode: resp.StatusCode, Method: cl.method, URL: cl.url, header: resp.Header}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	var payload struct {
		Message          string        `json:"message"`
		Errors           []ErrorDetail `json:"errors"`
		DocumentationURL string        `json:"documentation_url"`
	}
	if json.Unmarshal(data, &payload) == nil {
		ae.Message = c.redact(payload.Message)
		ae.DocumentationURL = c.redact(payload.DocumentationURL)
		for _, d := range payload.Errors {
			d.Message = c.redact(d.Message)
			ae.Errors = append(ae.Errors, d)
		}
	}
	return ae
}

// readJSON returns a handler decoding a size-limited JSON body into out.
func readJSON(out any) func(*http.Response) error {
	return func(resp *http.Response) error {
		data, err := readLimited(resp.Body, MaxResponseSize)
		if err != nil {
			return err
		}
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("github: invalid JSON response: %w", err)
		}
		return nil
	}
}

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("github: reading response: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("github: response exceeds %d bytes", limit)
	}
	return data, nil
}

// doJSON sends in (if non-nil) as JSON and decodes the response into out.
func (c *Client) doJSON(ctx context.Context, method, u string, in, out any) error {
	cl := &call{method: method, url: u, handle: readJSON(out)} //nolint:bodyclose // c.do closes the response body (drain) before returning
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("github: encoding request: %w", err)
		}
		cl.body = b
	}
	_, err := c.do(ctx, cl)
	return err
}

// listAll fetches every page of a list endpoint, following Link rel="next"
// only within the API origin.
func listAll[T any](ctx context.Context, c *Client, first string) ([]T, error) {
	out := []T{}
	next := first
	for page := 0; next != ""; page++ {
		if page >= maxPages {
			return nil, fmt.Errorf("github: GET %s: more than %d pages", first, maxPages)
		}
		var items []T
		h, err := c.do(ctx, &call{method: http.MethodGet, url: next, handle: readJSON(&items)}) //nolint:bodyclose // c.do closes the response body (drain) before returning
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		next = nextLink(h.Get("Link"))
		if next != "" {
			u, err := url.Parse(next)
			if err != nil || !sameOrigin(u, c.api) {
				return nil, fmt.Errorf("github: GET %s: refusing pagination link outside the API origin", first)
			}
		}
	}
	return out, nil
}

// nextLink extracts the rel="next" target of an RFC 8288 Link header.
func nextLink(h string) string {
	for _, part := range strings.Split(h, ",") {
		segs := strings.Split(part, ";")
		if len(segs) < 2 {
			continue
		}
		target := strings.TrimSpace(segs[0])
		if !strings.HasPrefix(target, "<") || !strings.HasSuffix(target, ">") {
			continue
		}
		for _, p := range segs[1:] {
			k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
			if ok && strings.EqualFold(strings.TrimSpace(k), "rel") {
				for _, rel := range strings.Fields(strings.Trim(strings.TrimSpace(v), `"`)) {
					if rel == "next" {
						return target[1 : len(target)-1]
					}
				}
			}
		}
	}
	return ""
}

// endpoint builds base + escaped segments + query. Each segment is escaped
// individually; use splitPath for values that may contain slashes.
func endpoint(base *url.URL, q url.Values, segs ...string) string {
	esc := make([]string, len(segs))
	for i, s := range segs {
		esc[i] = url.PathEscape(s)
	}
	u := base.Scheme + "://" + base.Host + base.EscapedPath() + "/" + strings.Join(esc, "/")
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

// splitPath splits a slash-separated value (a file path or ref) into
// segments, rejecting empty, "." and ".." segments.
func splitPath(what, p string) ([]string, error) {
	segs := strings.Split(p, "/")
	for _, s := range segs {
		if s == "" || s == "." || s == ".." {
			return nil, fmt.Errorf("github: invalid %s %q", what, p)
		}
	}
	return segs, nil
}

func checkRepo(owner, repo string) error {
	for _, s := range []string{owner, repo} {
		if s == "" || s == "." || s == ".." || strings.ContainsAny(s, "/\\?#%") {
			return fmt.Errorf("github: invalid repository %q", owner+"/"+repo)
		}
	}
	return nil
}

func repoURL(base *url.URL, owner, repo string, q url.Values, segs ...string) string {
	return endpoint(base, q, append([]string{"repos", owner, repo}, segs...)...)
}

func pageQuery() url.Values {
	return url.Values{"per_page": {strconv.Itoa(perPage)}}
}
