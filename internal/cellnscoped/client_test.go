package cellnscoped

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	cap "github.com/sympozium-ai/sympozium/internal/cellncapability"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestHostClientRequiresExplicitTLSAndDisablesAmbientRouting(t *testing.T) {
	dir := t.TempDir()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "scoped.test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644); err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte("operator-transport-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := newHostClient(EndpointConfig{URL: "https://scoped.test", CAFile: caPath, TokenFile: tokenPath})
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := c.http.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil || transport.TLSClientConfig == nil || transport.TLSClientConfig.RootCAs == nil || c.http.CheckRedirect == nil {
		t.Fatal("scoped client did not retain explicit CA/no-proxy/no-redirect transport")
	}
	if _, err := newHostClient(EndpointConfig{URL: "http://scoped.test", CAFile: caPath, TokenFile: tokenPath}); err == nil {
		t.Fatal("plaintext endpoint accepted")
	}
	if _, err := newHostClient(EndpointConfig{URL: "https://scoped.test/tenant/path", CAFile: caPath, TokenFile: tokenPath}); err == nil {
		t.Fatal("tenant-selectable base path accepted")
	}
}

func TestNativeStartSeparatesOperatorAndCapabilityCredentials(t *testing.T) {
	var gotBody []byte
	host := &hostClient{origin: "https://receiver.invalid", token: "operator-only", http: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v1/scoped/start" || r.Header.Get("Authorization") != "Bearer operator-only" || r.Header.Get("X-Celln-Execution-Permit") != "execution-only" || r.Header.Get("X-Celln-Model-Permit") != "model-only" {
			t.Fatalf("credential or endpoint separation failed: path=%s headers=%v", r.URL.Path, r.Header)
		}
		gotBody, _ = io.ReadAll(r.Body)
		return &http.Response{StatusCode: 202, Header: make(http.Header), Body: io.NopCloser(bytes.NewBufferString(`{"id":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","owner":"blake3:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","phase":"Admitted","cleanupConfirmed":false}`))}, nil
	})}}
	client := &NativeClient{host: host}
	if _, err := client.Start(context.Background(), "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", cap.NewToken("execution-only"), cap.NewToken("model-only")); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(gotBody, []byte("operator-only")) || bytes.Contains(gotBody, []byte("execution-only")) || bytes.Contains(gotBody, []byte("model-only")) {
		t.Fatal("transport or scoped permits leaked into the native request body")
	}
}
