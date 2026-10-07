package netutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// tlsConfigWith returns a TLS client config with the given minimum version.
func tlsConfigWith(t *testing.T, version uint16) *tls.Config {
	t.Helper()
	return &tls.Config{MinVersion: version}
}

// mustRequest builds a GET request for proxy-function assertions.
func mustRequest(t *testing.T, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	return req
}

// testPEM writes a self-signed certificate (private CA material) to a temp
// file and returns its path.
func testPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "nenya-test-ca"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IsCA:         true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write pem: %v", err)
	}
	return path
}

func TestApplyTransportSecurity_FloorAndPreserve(t *testing.T) {
	t.Run("sets the floor on a bare transport", func(t *testing.T) {
		tr := &http.Transport{}
		ApplyTransportSecurity(tr)
		if tr.TLSClientConfig == nil || tr.TLSClientConfig.MinVersion != MinTLSVersion {
			t.Fatalf("expected MinVersion %d, got %#v", MinTLSVersion, tr.TLSClientConfig)
		}
	})
	t.Run("preserves a stricter floor", func(t *testing.T) {
		tr := &http.Transport{TLSClientConfig: tlsConfigWith(t, tls.VersionTLS13)}
		ApplyTransportSecurity(tr)
		if tr.TLSClientConfig.MinVersion != tls.VersionTLS13 {
			t.Errorf("stricter floor downgraded to %d", tr.TLSClientConfig.MinVersion)
		}
	})
	t.Run("nil transport is a no-op", func(t *testing.T) {
		ApplyTransportSecurity(nil)
	})
}

func TestLoadCABundle(t *testing.T) {
	t.Run("valid pem loads into system pool", func(t *testing.T) {
		pool, err := LoadCABundle(testPEM(t))
		if err != nil {
			t.Fatalf("LoadCABundle: %v", err)
		}
		if pool == nil {
			t.Fatal("expected a pool")
		}
	})
	t.Run("missing file errors", func(t *testing.T) {
		if _, err := LoadCABundle(filepath.Join(t.TempDir(), "missing.pem")); err == nil {
			t.Fatal("expected error for missing bundle")
		}
	})
	t.Run("non-PEM content errors", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "junk.pem")
		if err := os.WriteFile(path, []byte("not a certificate"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadCABundle(path); err == nil {
			t.Fatal("expected error for junk bundle")
		}
	})
	t.Run("empty path errors", func(t *testing.T) {
		if _, err := LoadCABundle(""); err == nil {
			t.Fatal("expected error for empty path")
		}
	})
}

func TestValidateProxyURL(t *testing.T) {
	for _, raw := range []string{"http://proxy:8080", "https://proxy", "socks5://proxy:1080", "socks5h://proxy:1080"} {
		if err := ValidateProxyURL(raw); err != nil {
			t.Errorf("ValidateProxyURL(%q) = %v, want nil", raw, err)
		}
	}
	for _, raw := range []string{"", "ftp://proxy", "::not-a-url", "proxy:1080"} {
		if err := ValidateProxyURL(raw); err == nil {
			t.Errorf("ValidateProxyURL(%q) = nil, want error", raw)
		}
	}
}

func TestProxyOrDefault(t *testing.T) {
	t.Run("empty falls back to environment", func(t *testing.T) {
		proxy, err := ProxyOrDefault("")
		if err != nil {
			t.Fatalf("ProxyOrDefault(\"\"): %v", err)
		}
		if proxy == nil {
			t.Fatal("expected a non-nil proxy func (environment-backed)")
		}
	})
	t.Run("explicit wins", func(t *testing.T) {
		proxy, err := ProxyOrDefault("http://egress:3128")
		if err != nil {
			t.Fatalf("ProxyOrDefault: %v", err)
		}
		req := mustRequest(t, "https://api.example.com/v1")
		u, err2 := proxy(req)
		if err2 != nil {
			t.Fatalf("proxy(req): %v", err2)
		}
		if u == nil || u.Host != "egress:3128" {
			t.Fatalf("expected egress:3128, got %v", u)
		}
	})
	t.Run("invalid errors", func(t *testing.T) {
		if _, err := ProxyOrDefault("gopher://egress"); err == nil {
			t.Fatal("expected error for unsupported scheme")
		}
	})
}
