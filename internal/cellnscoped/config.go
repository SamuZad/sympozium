// Package cellnscoped implements the operator-enabled, one-shot native Celln
// control path. It is deliberately separate from the legacy router protocol.
package cellnscoped

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/sympozium-ai/sympozium/internal/cellnauthority"
	cap "github.com/sympozium-ai/sympozium/internal/cellncapability"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type EndpointConfig struct {
	URL       string `json:"url"`
	CAFile    string `json:"caFile"`
	TokenFile string `json:"tokenFile"`
}

type IssuerConfig struct {
	Name           string `json:"name"`
	KeyID          string `json:"keyId"`
	PrivateKeyFile string `json:"privateKeyFile"`
}

// Config is operator-owned process configuration. Absence of the configuration
// file disables scoped dispatch; none of these values are tenant selectable.
type Config struct {
	ClusterID            string          `json:"clusterId"`
	PreparationNamespace string          `json:"preparationNamespace"`
	Receiver             EndpointConfig  `json:"receiver"`
	Gateway              *EndpointConfig `json:"gateway"`
	Issuer               IssuerConfig    `json:"issuer"`
}

// LoadDispatcher validates all operator configuration at process startup. It
// reads only explicitly named host files and retains transport keys in memory;
// they are never placed in prepared records or capability decisions.
func LoadDispatcher(path string, writer client.Client, reader client.Reader) (*Dispatcher, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("scoped controller configuration path is required")
	}
	raw, err := readBoundedFile(path, 128<<10, false)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := cap.StrictDecode(raw, &cfg); err != nil {
		return nil, fmt.Errorf("invalid scoped controller configuration: %w", err)
	}
	if cfg.ClusterID == "" || cfg.PreparationNamespace == "" || cfg.Issuer.Name == "" || cfg.Issuer.KeyID == "" || cfg.Issuer.PrivateKeyFile == "" {
		return nil, errors.New("scoped controller configuration is incomplete")
	}
	receiver, err := NewNativeClient(cfg.Receiver)
	if err != nil {
		return nil, fmt.Errorf("invalid scoped receiver configuration: %w", err)
	}
	var gateway *GatewayClient
	if cfg.Gateway != nil {
		gateway, err = NewGatewayClient(*cfg.Gateway)
		if err != nil {
			return nil, fmt.Errorf("invalid model gateway configuration: %w", err)
		}
	}
	key, err := loadPrivateKey(cfg.Issuer.PrivateKeyFile, cfg.Issuer.KeyID)
	if err != nil {
		return nil, err
	}
	issuer, err := cap.NewIssuer(cfg.Issuer.Name, key, nil)
	if err != nil {
		return nil, err
	}
	store := cellnauthority.PreparedStore{Writer: writer, Reader: reader, Namespace: cfg.PreparationNamespace, Resolver: cellnauthority.PlatformResolver{Reader: reader}}
	return &Dispatcher{ClusterID: cfg.ClusterID, Store: store, Receiver: receiver, Gateway: gateway, Issuer: issuer}, nil
}

func readBoundedFile(path string, limit int64, private bool) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > limit || (private && info.Mode().Perm()&0077 != 0) {
		return nil, errors.New("operator file must be bounded, regular, and have the required permissions")
	}
	return os.ReadFile(path)
}

func loadPrivateKey(path, keyID string) (cap.SigningKey, error) {
	raw, err := readBoundedFile(path, 16<<10, true)
	if err != nil {
		return cap.SigningKey{}, fmt.Errorf("load capability signing key: %w", err)
	}
	block, rest := pem.Decode(raw)
	if block == nil || block.Type != "PRIVATE KEY" || len(strings.TrimSpace(string(rest))) != 0 {
		return cap.SigningKey{}, errors.New("capability signing key must be one PKCS#8 PRIVATE KEY PEM block")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return cap.SigningKey{}, errors.New("invalid capability signing key")
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok || len(key) != ed25519.PrivateKeySize {
		return cap.SigningKey{}, errors.New("capability signing key must be Ed25519")
	}
	return cap.SigningKey{KeyID: keyID, PrivateKey: key}, nil
}
