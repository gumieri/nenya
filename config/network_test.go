package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// validTestPEM writes a self-signed certificate and returns its path —
// AppendCertsFromPEM requires parseable DER, so a hand-written stub fails.
// Local copy: internal/testutil imports config, which this package cannot
// import back (internal/netutil keeps a third copy to stay a stdlib-only
// leaf). Do not "deduplicate" across these boundaries.
func validTestPEM(t *testing.T) string {
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

// TestValidateNetworkPolicy covers the NENYA-137 startup validation: CA
// bundles must be readable PEM, proxy URLs must use a supported scheme —
// checked on the network section and per provider.
func TestValidateNetworkPolicy(t *testing.T) {
	goodPEMFile := validTestPEM(t)
	badPEMFile := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(badPEMFile, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name      string
		cfg       func(c *Config)
		wantError bool
	}{
		{
			name:      "empty network and providers",
			cfg:       func(c *Config) {},
			wantError: false,
		},
		{
			name: "valid network section",
			cfg:  func(c *Config) { c.Network = &NetworkConfig{ProxyURL: "http://egress:3128"} },
		},
		{
			name:      "network bad proxy scheme",
			cfg:       func(c *Config) { c.Network = &NetworkConfig{ProxyURL: "gopher://egress"} },
			wantError: true,
		},
		{
			name:      "network missing ca bundle file",
			cfg:       func(c *Config) { c.Network = &NetworkConfig{CABundle: filepath.Join(t.TempDir(), "nope.pem")} },
			wantError: true,
		},
		{
			name:      "network ca bundle not PEM",
			cfg:       func(c *Config) { c.Network = &NetworkConfig{CABundle: badPEMFile} },
			wantError: true,
		},
		{
			name: "provider bad proxy scheme",
			cfg: func(c *Config) {
				c.Providers = map[string]ProviderConfig{}
				c.Providers["x"] = ProviderConfig{ProxyURL: "gopher://egress"}
			},
			wantError: true,
		},
		{
			name: "provider valid ca bundle file accepted",
			cfg: func(c *Config) {
				c.Providers = map[string]ProviderConfig{}
				c.Providers["x"] = ProviderConfig{CABundle: goodPEMFile}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{}
			tc.cfg(cfg)
			errs := validateNetworkPolicy(cfg)
			if tc.wantError && len(errs) == 0 {
				t.Fatalf("expected validation errors, got none")
			}
			if !tc.wantError && len(errs) != 0 {
				t.Fatalf("unexpected validation errors: %v", errs)
			}
			for _, e := range errs {
				if !strings.Contains(e, "ca_bundle") && !strings.Contains(e, "proxy_url") {
					t.Errorf("error %q should name the offending field", e)
				}
			}
		})
	}
}
