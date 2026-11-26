package httpclient

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"trimtab.io/trimtab/pkg/config"
)

func TestClientAuthModesAndHeaders(t *testing.T) {
	t.Run("bearer token from env + tenantID + User-Agent", func(t *testing.T) {
		t.Setenv("TEST_TRIMTAB_BEARER", "super-secret-token")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("Authorization"); got != "Bearer super-secret-token" {
				t.Errorf("Authorization = %q, want Bearer super-secret-token", got)
			}
			if got := r.Header.Get("X-Scope-OrgID"); got != "tenant-alpha" {
				t.Errorf("X-Scope-OrgID = %q, want tenant-alpha", got)
			}
			if got := r.Header.Get("User-Agent"); !strings.HasPrefix(got, "trimtab/") {
				t.Errorf("User-Agent = %q, want prefix trimtab/", got)
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		defer srv.Close()

		c, err := New(Options{
			BaseURL:  srv.URL,
			TenantID: "tenant-alpha",
			Auth:     config.HTTPAuthConfig{BearerTokenEnv: "TEST_TRIMTAB_BEARER"},
		})
		if err != nil {
			t.Fatal(err)
		}
		body, err := c.Get(context.Background(), "/api/v1/status")
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != `{"ok":true}` {
			t.Fatalf("unexpected body %q", string(body))
		}
	})

	t.Run("basic auth from env and file", func(t *testing.T) {
		dir := t.TempDir()
		passFile := filepath.Join(dir, "pass")
		if err := os.WriteFile(passFile, []byte("s3cr3t-pass\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("TEST_TRIMTAB_USER", "metrics-reader")

		wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("metrics-reader:s3cr3t-pass"))
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("Authorization"); got != wantAuth {
				t.Errorf("Authorization = %q, want %q", got, wantAuth)
			}
			_, _ = w.Write([]byte("ok"))
		}))
		defer srv.Close()

		c, err := New(Options{
			BaseURL: srv.URL,
			Auth: config.HTTPAuthConfig{
				BasicAuth: &config.BasicAuthConfig{
					UsernameEnv:  "TEST_TRIMTAB_USER",
					PasswordFile: passFile,
				},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Get(context.Background(), "/"); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("custom headers env and file + hot reload", func(t *testing.T) {
		dir := t.TempDir()
		tokFile := filepath.Join(dir, "token")
		if err := os.WriteFile(tokFile, []byte("v1-token"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("TEST_TRIMTAB_KEY", "env-key-123")

		var seenHeader string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seenHeader = r.Header.Get("X-API-Key")
			if r.Header.Get("X-Env-Key") != "env-key-123" {
				t.Errorf("missing X-Env-Key")
			}
			_, _ = w.Write([]byte("ok"))
		}))
		defer srv.Close()

		c, err := New(Options{
			BaseURL:       srv.URL,
			HeadersEnv:    map[string]string{"X-Env-Key": "TEST_TRIMTAB_KEY"},
			HeadersFile:   map[string]string{"X-API-Key": tokFile},
			FileReloadTTL: 20 * time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Get(context.Background(), "/"); err != nil {
			t.Fatal(err)
		}
		if seenHeader != "v1-token" {
			t.Fatalf("expected v1-token, got %q", seenHeader)
		}

		// Rotate secret file and wait past TTL
		if err := os.WriteFile(tokFile, []byte("v2-rotated-token"), 0o600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(35 * time.Millisecond)
		if _, err := c.Get(context.Background(), "/"); err != nil {
			t.Fatal(err)
		}
		if seenHeader != "v2-rotated-token" {
			t.Fatalf("expected hot-reloaded v2-rotated-token, got %q", seenHeader)
		}
	})
}

func TestClientValidationFailClosedAndRedaction(t *testing.T) {
	// Missing env var fails closed immediately
	if _, err := New(Options{
		BaseURL: "http://127.0.0.1:1",
		Auth:    config.HTTPAuthConfig{BearerTokenEnv: "TRIMTAB_NONEXISTENT_ENV_VAR"},
	}); err == nil {
		t.Fatal("expected error when bearerTokenEnv is unset")
	}

	// Empty file fails closed
	dir := t.TempDir()
	emptyFile := filepath.Join(dir, "empty")
	if err := os.WriteFile(emptyFile, []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Options{
		BaseURL: "http://127.0.0.1:1",
		Auth:    config.HTTPAuthConfig{BearerTokenFile: emptyFile},
	}); err == nil {
		t.Fatal("expected error when bearerTokenFile is empty")
	}

	// Mutual exclusion fails closed
	if _, err := New(Options{
		BaseURL: "http://127.0.0.1:1",
		Auth: config.HTTPAuthConfig{
			BearerToken: "abc",
			BasicAuth:   &config.BasicAuthConfig{Username: "u", Password: "p"},
		},
	}); err == nil {
		t.Fatal("expected mutual exclusion error for bearerToken + basicAuth")
	}

	// Secret redaction in error bodies
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream rejected token top-secret-xyz", http.StatusBadRequest)
	}))
	defer srv.Close()

	c, err := New(Options{
		BaseURL: srv.URL,
		Auth:    config.HTTPAuthConfig{BearerToken: "top-secret-xyz"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Get(context.Background(), "/err")
	if err == nil {
		t.Fatal("expected HTTP 400 error")
	}
	if strings.Contains(err.Error(), "top-secret-xyz") {
		t.Fatalf("secret leaked in error string: %v", err)
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("expected [REDACTED] in error string: %v", err)
	}
}

func TestClientRetrySemantics(t *testing.T) {
	t.Run("429 and 503 retry up to success", func(t *testing.T) {
		var attempts atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			n := attempts.Add(1)
			if n == 1 {
				w.Header().Set("Retry-After", "0")
				http.Error(w, "rate limited", http.StatusTooManyRequests)
				return
			}
			if n == 2 {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte("recovered"))
		}))
		defer srv.Close()

		c, err := New(Options{
			BaseURL:     srv.URL,
			MaxRetries:  3,
			BaseBackoff: 5 * time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		body, err := c.Get(context.Background(), "/ok")
		if err != nil {
			t.Fatalf("expected retry recovery, got %v", err)
		}
		if string(body) != "recovered" || attempts.Load() != 3 {
			t.Fatalf("unexpected body=%q attempts=%d", string(body), attempts.Load())
		}
	})

	t.Run("401 and 403 never retry", func(t *testing.T) {
		for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
			var attempts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				attempts.Add(1)
				http.Error(w, "denied", code)
			}))

			c, err := New(Options{
				BaseURL:     srv.URL,
				MaxRetries:  3,
				BaseBackoff: 5 * time.Millisecond,
			})
			if err != nil {
				srv.Close()
				t.Fatal(err)
			}
			_, err = c.Get(context.Background(), "/auth")
			srv.Close()
			var httpErr *HTTPError
			if !errors.As(err, &httpErr) || httpErr.StatusCode != code {
				t.Fatalf("expected HTTPError %d, got %v", code, err)
			}
			if attempts.Load() != 1 {
				t.Fatalf("HTTP %d must NOT retry, got %d attempts", code, attempts.Load())
			}
		}
	})
}

func TestClientTLSmTLSAndInsecureWarning(t *testing.T) {
	dir := t.TempDir()
	caCertPEM, caCert, caKey := generateSelfSignedCA(t)
	srvCertPEM, srvKeyPEM := generateLeafCert(t, caCert, caKey, "127.0.0.1", true)
	cliCertPEM, cliKeyPEM := generateLeafCert(t, caCert, caKey, "trimtab-client", false)

	caFile := filepath.Join(dir, "ca.crt")
	cliCertFile := filepath.Join(dir, "client.crt")
	cliKeyFile := filepath.Join(dir, "client.key")
	if err := os.WriteFile(caFile, caCertPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cliCertFile, cliCertPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cliKeyFile, cliKeyPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	srvKP, err := tls.X509KeyPair(srvCertPEM, srvKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(caCertPEM)

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("mtls-ok"))
	}))
	srv.TLS = &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{srvKP},
		ClientCAs:    caPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}
	srv.StartTLS()
	defer srv.Close()

	// Valid mTLS passes
	c, err := New(Options{
		BaseURL: srv.URL,
		Auth: config.HTTPAuthConfig{
			TLS: &config.TLSConfig{
				CAFile:   caFile,
				CertFile: cliCertFile,
				KeyFile:  cliKeyFile,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Warnings()) != 0 {
		t.Fatalf("unexpected warnings for verified mTLS: %v", c.Warnings())
	}
	body, err := c.Get(context.Background(), "/mtls")
	if err != nil || string(body) != "mtls-ok" {
		t.Fatalf("mTLS GET failed: body=%q err=%v", string(body), err)
	}

	// InsecureSkipVerify emits visible warning
	cInsecure, err := New(Options{
		BaseURL: srv.URL,
		Auth: config.HTTPAuthConfig{
			TLS: &config.TLSConfig{
				CertFile:           cliCertFile,
				KeyFile:            cliKeyFile,
				InsecureSkipVerify: true,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cInsecure.Warnings()) == 0 || !strings.Contains(cInsecure.Warnings()[0], "insecureSkipVerify") {
		t.Fatalf("expected insecureSkipVerify warning, got %v", cInsecure.Warnings())
	}
}

func generateSelfSignedCA(t *testing.T) ([]byte, *x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "trimtab-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return certPEM, cert, key
}

func generateLeafCert(t *testing.T, caCert *x509.Certificate, caKey *ecdsa.PrivateKey, cn string, isServer bool) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
	}
	if isServer {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func TestClientPutSuccessAndSecretRedaction(t *testing.T) {
	t.Setenv("TEST_PUSH_TOK", "secret-push-token-99")
	var gotMethod, gotCT, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotCT = r.Header.Get("Content-Type")
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c, err := New(Options{
		BaseURL: srv.URL,
		Auth:    config.HTTPAuthConfig{BearerTokenEnv: "TEST_PUSH_TOK"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Put(context.Background(), "/metrics/job/trimtab", "text/plain; version=0.0.4", []byte("trimtab_active_series 42\n")); err != nil {
		t.Fatalf("Put failed: %v", err)
	}
	if gotMethod != http.MethodPut || gotCT != "text/plain; version=0.0.4" || gotAuth != "Bearer secret-push-token-99" || gotBody != "trimtab_active_series 42\n" {
		t.Fatalf("unexpected PUT request: method=%q ct=%q auth=%q body=%q", gotMethod, gotCT, gotAuth, gotBody)
	}

	// 401 failure must redact secret from error
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "denied secret-push-token-99", http.StatusUnauthorized)
	}))
	defer badSrv.Close()
	cBad, err := New(Options{
		BaseURL: badSrv.URL,
		Auth:    config.HTTPAuthConfig{BearerTokenEnv: "TEST_PUSH_TOK"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cBad.Put(context.Background(), "/metrics/job/trimtab", "text/plain", []byte("x 1\n")); err == nil || strings.Contains(err.Error(), "secret-push-token-99") || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("expected redacted 401 error, got %v", err)
	}
}
