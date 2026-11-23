package httpclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"trimtab.io/trimtab/pkg/config"
)

// DefaultUserAgent is stamped on every outbound HTTP request so query-log sources
// can automatically exclude Trimtab's own inspection queries (BACKENDS_AND_AUTH §5.3).
var DefaultUserAgent = "trimtab/dev"

const (
	defaultTimeout       = 30 * time.Second
	defaultMaxRetries    = 3
	defaultBaseBackoff   = 200 * time.Millisecond
	defaultFileReloadTTL = 30 * time.Second
	maxRetryAfter        = 5 * time.Second
)

// Options configures a shared read-only HTTP client for a telemetry backend.
type Options struct {
	BaseURL       string
	Timeout       time.Duration
	TenantID      string
	Headers       map[string]string
	HeadersEnv    map[string]string
	HeadersFile   map[string]string
	Auth          config.HTTPAuthConfig
	UserAgent     string
	MaxRetries    int
	BaseBackoff   time.Duration
	FileReloadTTL time.Duration
}

// HTTPError represents a non-2xx HTTP response with secrets redacted.
type HTTPError struct {
	StatusCode int
	Method     string
	Endpoint   string
	Body       string
}

func (e *HTTPError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("%s %s returned HTTP %d", e.Method, e.Endpoint, e.StatusCode)
	}
	return fmt.Sprintf("%s %s returned HTTP %d: %s", e.Method, e.Endpoint, e.StatusCode, e.Body)
}

type resolvedSecrets struct {
	bearerToken   string
	basicUsername string
	basicPassword string
	headers       map[string]string
	secretValues  []string
	loadedAt      time.Time
}

// Client is a lean, stdlib-only HTTP client with unified auth, TLS/mTLS,
// secret hot-reload & redaction, bounded retries, and self-query User-Agent stamping.
type Client struct {
	baseURL       string
	tenantID      string
	staticHeaders map[string]string
	headersEnv    map[string]string
	headersFile   map[string]string
	auth          config.HTTPAuthConfig
	userAgent     string
	maxRetries    int
	baseBackoff   time.Duration
	fileReloadTTL time.Duration
	httpClient    *http.Client
	warnings      []string

	mu      sync.RWMutex
	secrets resolvedSecrets
}

// New validates options and secrets fail-closed, builds TLS/mTLS transport if configured,
// and returns a ready-to-use Client.
func New(opts Options) (*Client, error) {
	if err := opts.Auth.Validate("auth"); err != nil {
		return nil, err
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	maxRetries := opts.MaxRetries
	if maxRetries <= 0 {
		maxRetries = defaultMaxRetries
	}
	baseBackoff := opts.BaseBackoff
	if baseBackoff <= 0 {
		baseBackoff = defaultBaseBackoff
	}
	reloadTTL := opts.FileReloadTTL
	if reloadTTL <= 0 {
		reloadTTL = defaultFileReloadTTL
	}
	ua := strings.TrimSpace(opts.UserAgent)
	if ua == "" {
		ua = DefaultUserAgent
	}
	if !strings.HasPrefix(strings.ToLower(ua), "trimtab/") {
		ua = "trimtab/" + ua
	}

	var warnings []string
	var transport *http.Transport
	if opts.Auth.TLS != nil {
		tlsCfg, tlsWarns, err := buildTLSConfig(opts.Auth.TLS)
		if err != nil {
			return nil, err
		}
		warnings = append(warnings, tlsWarns...)
		transport = http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = tlsCfg
	}

	httpClient := &http.Client{Timeout: timeout}
	if transport != nil {
		httpClient.Transport = transport
	}

	c := &Client{
		baseURL:       strings.TrimRight(opts.BaseURL, "/"),
		tenantID:      strings.TrimSpace(opts.TenantID),
		staticHeaders: cloneMap(opts.Headers),
		headersEnv:    cloneMap(opts.HeadersEnv),
		headersFile:   cloneMap(opts.HeadersFile),
		auth:          opts.Auth,
		userAgent:     ua,
		maxRetries:    maxRetries,
		baseBackoff:   baseBackoff,
		fileReloadTTL: reloadTTL,
		httpClient:    httpClient,
		warnings:      warnings,
	}

	// Eagerly resolve and validate all *Env and *File secrets fail-closed.
	sec, err := c.resolveAllSecrets()
	if err != nil {
		return nil, err
	}
	c.secrets = sec
	return c, nil
}

// Warnings returns non-fatal client configuration warnings (e.g. insecureSkipVerify).
func (c *Client) Warnings() []string {
	out := make([]string, len(c.warnings))
	copy(out, c.warnings)
	return out
}

// Redact replaces any resolved secret values (plus Bearer/Basic/Cookie values) with [REDACTED].
func (c *Client) Redact(s string) string {
	if s == "" {
		return ""
	}
	c.mu.RLock()
	vals := append([]string(nil), c.secrets.secretValues...)
	c.mu.RUnlock()

	out := s
	for _, v := range vals {
		if v != "" {
			out = strings.ReplaceAll(out, v, "[REDACTED]")
		}
	}
	return out
}

// Get performs an idempotent read-only HTTP GET to baseURL+endpoint with bounded retries
// on transient statuses (429/502/503/504) and zero retries on 401/403.
func (c *Client) Get(ctx context.Context, endpoint string) ([]byte, error) {
	body, _, err := c.GetWithStatus(ctx, endpoint)
	return body, err
}

// GetWithStatus performs an idempotent read-only HTTP GET and returns the response body and status code.
func (c *Client) GetWithStatus(ctx context.Context, endpoint string) ([]byte, int, error) {
	sec, err := c.currentSecrets()
	if err != nil {
		return nil, 0, fmt.Errorf("load backend credentials: %w", err)
	}

	targetURL := c.baseURL + endpoint
	if strings.HasPrefix(endpoint, "http://") || strings.HasPrefix(endpoint, "https://") {
		targetURL = endpoint
	}

	var lastErr error
	var lastStatus int
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
		if err != nil {
			return nil, 0, fmt.Errorf("build GET %s: %w", endpoint, err)
		}
		c.applyHeaders(req, sec)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("GET %s: %s", endpoint, c.Redact(err.Error()))
			if attempt == c.maxRetries || ctx.Err() != nil {
				return nil, 0, lastErr
			}
			if sleepErr := sleepWithContext(ctx, c.backoffDelay(attempt, "")); sleepErr != nil {
				return nil, 0, lastErr
			}
			continue
		}

		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		lastStatus = resp.StatusCode
		if readErr != nil {
			lastErr = fmt.Errorf("read %s: %s", endpoint, c.Redact(readErr.Error()))
			if attempt == c.maxRetries || ctx.Err() != nil {
				return nil, lastStatus, lastErr
			}
			if sleepErr := sleepWithContext(ctx, c.backoffDelay(attempt, "")); sleepErr != nil {
				return nil, lastStatus, lastErr
			}
			continue
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return body, resp.StatusCode, nil
		}

		httpErr := &HTTPError{
			StatusCode: resp.StatusCode,
			Method:     http.MethodGet,
			Endpoint:   endpoint,
			Body:       c.Redact(strings.TrimSpace(string(body))),
		}

		// Never retry 401/403 or other non-transient 4xx errors.
		if !isRetryableStatus(resp.StatusCode) || attempt == c.maxRetries {
			return nil, resp.StatusCode, httpErr
		}

		delay := c.backoffDelay(attempt, resp.Header.Get("Retry-After"))
		if sleepErr := sleepWithContext(ctx, delay); sleepErr != nil {
			return nil, resp.StatusCode, httpErr
		}
		lastErr = httpErr
	}
	return nil, lastStatus, lastErr
}

func (c *Client) applyHeaders(req *http.Request, sec resolvedSecrets) {
	req.Header.Set("User-Agent", c.userAgent)
	if c.tenantID != "" {
		req.Header.Set("X-Scope-OrgID", c.tenantID)
	}
	for k, v := range c.staticHeaders {
		req.Header.Set(k, v)
	}
	for k, v := range sec.headers {
		req.Header.Set(k, v)
	}
	if sec.bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+sec.bearerToken)
	} else if sec.basicUsername != "" || sec.basicPassword != "" {
		creds := base64.StdEncoding.EncodeToString([]byte(sec.basicUsername + ":" + sec.basicPassword))
		req.Header.Set("Authorization", "Basic "+creds)
	}
}

func (c *Client) currentSecrets() (resolvedSecrets, error) {
	c.mu.RLock()
	cached := c.secrets
	hasFiles := c.hasFileBackedSecrets()
	if !hasFiles || time.Since(cached.loadedAt) < c.fileReloadTTL {
		c.mu.RUnlock()
		return cached, nil
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.secrets.loadedAt) < c.fileReloadTTL {
		return c.secrets, nil
	}
	fresh, err := c.resolveAllSecrets()
	if err != nil {
		return resolvedSecrets{}, err
	}
	c.secrets = fresh
	return fresh, nil
}

func (c *Client) hasFileBackedSecrets() bool {
	if c.auth.BearerTokenFile != "" {
		return true
	}
	if c.auth.BasicAuth != nil && (c.auth.BasicAuth.UsernameFile != "" || c.auth.BasicAuth.PasswordFile != "") {
		return true
	}
	if len(c.headersFile) > 0 || len(c.auth.HeadersFile) > 0 {
		return true
	}
	return false
}

func (c *Client) resolveAllSecrets() (resolvedSecrets, error) {
	sec := resolvedSecrets{
		headers:  make(map[string]string),
		loadedAt: time.Now(),
	}
	var secretSet []string

	bearer, isSecret, err := resolveValue("bearerToken", c.auth.BearerToken, c.auth.BearerTokenEnv, c.auth.BearerTokenFile)
	if err != nil {
		return sec, err
	}
	if bearer != "" {
		sec.bearerToken = bearer
		if isSecret || c.auth.BearerToken != "" {
			secretSet = append(secretSet, bearer)
		}
	}

	if c.auth.BasicAuth != nil {
		user, uSec, err := resolveValue("basicAuth.username", c.auth.BasicAuth.Username, c.auth.BasicAuth.UsernameEnv, c.auth.BasicAuth.UsernameFile)
		if err != nil {
			return sec, err
		}
		pass, pSec, err := resolveValue("basicAuth.password", c.auth.BasicAuth.Password, c.auth.BasicAuth.PasswordEnv, c.auth.BasicAuth.PasswordFile)
		if err != nil {
			return sec, err
		}
		sec.basicUsername = user
		sec.basicPassword = pass
		if user != "" && (uSec || c.auth.BasicAuth.Username != "") {
			secretSet = append(secretSet, user)
		}
		if pass != "" && (pSec || c.auth.BasicAuth.Password != "") {
			secretSet = append(secretSet, pass)
		}
		if user != "" && pass != "" {
			secretSet = append(secretSet, base64.StdEncoding.EncodeToString([]byte(user+":"+pass)))
		}
	}

	loadHeaderMap := func(m map[string]string, isFile bool) error {
		for hdr, ref := range m {
			var val string
			if isFile {
				raw, err := os.ReadFile(ref)
				if err != nil {
					return fmt.Errorf("read headersFile[%q] (%s): %w", hdr, ref, err)
				}
				val = strings.TrimSpace(string(raw))
				if val == "" {
					return fmt.Errorf("headersFile[%q] (%s) is empty", hdr, ref)
				}
			} else {
				v, ok := os.LookupEnv(ref)
				if !ok || strings.TrimSpace(v) == "" {
					return fmt.Errorf("headersEnv[%q] environment variable %q is unset or empty", hdr, ref)
				}
				val = strings.TrimSpace(v)
			}
			sec.headers[hdr] = val
			secretSet = append(secretSet, val)
		}
		return nil
	}

	if err := loadHeaderMap(c.auth.HeadersEnv, false); err != nil {
		return sec, err
	}
	if err := loadHeaderMap(c.headersEnv, false); err != nil {
		return sec, err
	}
	if err := loadHeaderMap(c.auth.HeadersFile, true); err != nil {
		return sec, err
	}
	if err := loadHeaderMap(c.headersFile, true); err != nil {
		return sec, err
	}

	// Sort longest-first so compound secrets (e.g. base64 basic auth) redact cleanly.
	sort.Slice(secretSet, func(i, j int) bool {
		if len(secretSet[i]) != len(secretSet[j]) {
			return len(secretSet[i]) > len(secretSet[j])
		}
		return secretSet[i] < secretSet[j]
	})
	sec.secretValues = secretSet
	return sec, nil
}

func resolveValue(field, literal, envName, filePath string) (val string, fromExternal bool, err error) {
	if envName != "" {
		v, ok := os.LookupEnv(envName)
		if !ok || strings.TrimSpace(v) == "" {
			return "", true, fmt.Errorf("%s env var %q is unset or empty", field, envName)
		}
		return strings.TrimSpace(v), true, nil
	}
	if filePath != "" {
		raw, err := os.ReadFile(filePath)
		if err != nil {
			return "", true, fmt.Errorf("read %s file %s: %w", field, filePath, err)
		}
		trimmed := strings.TrimSpace(string(raw))
		if trimmed == "" {
			return "", true, fmt.Errorf("%s file %s is empty", field, filePath)
		}
		return trimmed, true, nil
	}
	return literal, false, nil
}

func buildTLSConfig(cfg *config.TLSConfig) (*tls.Config, []string, error) {
	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: cfg.ServerName,
	}
	var warnings []string
	if cfg.InsecureSkipVerify {
		tlsCfg.InsecureSkipVerify = true
		warnings = append(warnings, "TLS insecureSkipVerify is enabled; server certificates are not verified")
	}
	if cfg.CAFile != "" {
		pemBytes, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, nil, fmt.Errorf("read TLS caFile %s: %w", cfg.CAFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, nil, fmt.Errorf("parse TLS caFile %s: no valid PEM certificates found", cfg.CAFile)
		}
		tlsCfg.RootCAs = pool
	}
	if cfg.CertFile != "" || cfg.KeyFile != "" {
		if cfg.CertFile == "" || cfg.KeyFile == "" {
			return nil, nil, errors.New("TLS certFile and keyFile must both be provided for mTLS")
		}
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, nil, fmt.Errorf("load mTLS keypair (%s, %s): %w", cfg.CertFile, cfg.KeyFile, err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}
	return tlsCfg, warnings, nil
}

func isRetryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func (c *Client) backoffDelay(attempt int, retryAfterHeader string) time.Duration {
	if retryAfterHeader != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(retryAfterHeader)); err == nil && secs >= 0 {
			d := time.Duration(secs) * time.Second
			if d > maxRetryAfter {
				return maxRetryAfter
			}
			return d
		}
		if t, err := http.ParseTime(retryAfterHeader); err == nil {
			d := time.Until(t)
			if d < 0 {
				d = 0
			}
			if d > maxRetryAfter {
				return maxRetryAfter
			}
			return d
		}
	}
	d := c.baseBackoff << attempt
	jitter := time.Duration(rand.Int63n(int64(c.baseBackoff/2 + 1)))
	d += jitter
	if d > maxRetryAfter {
		return maxRetryAfter
	}
	return d
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
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

func cloneMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
