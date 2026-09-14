package cellninstall

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PreflightTimeout bounds one probe request to a backend.
const PreflightTimeout = 30 * time.Second

// PreflightBackend sends the smallest real chat request a backend accepts
// (one output token) with the key the fleet will use, so a dead provider,
// a wrong endpoint or a bad key is reported at install time instead of as a
// lost parent later. A models listing is not enough: a provider can list
// models while refusing completions, as DeepSeek did during a 503 outage.
func PreflightBackend(ctx context.Context, httpClient *http.Client, b FleetBackend, credential string) error {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: PreflightTimeout}
	}
	var body map[string]any
	headers := map[string]string{"Content-Type": "application/json"}
	switch b.Model.Protocol {
	case "anthropic-messages":
		body = map[string]any{"model": b.Model.Name, "max_tokens": 1, "messages": []map[string]string{{"role": "user", "content": "ping"}}}
		headers["x-api-key"] = credential
		headers["anthropic-version"] = "2023-06-01"
	default:
		body = map[string]any{"model": b.Model.Name, "max_tokens": 1, "messages": []map[string]string{{"role": "user", "content": "ping"}}}
		headers["Authorization"] = "Bearer " + credential
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, PreflightTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.Model.Endpoint, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("backend %s: %w", b.Name, err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("backend %s: %s is unreachable from here: %w (use --celln-fleet-skip-preflight if only the nodes can reach it)", b.Name, b.Model.Endpoint, err)
	}
	defer resp.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("backend %s: %s refused the key (HTTP %d): %s", b.Name, b.Model.Endpoint, resp.StatusCode, strings.TrimSpace(string(snippet)))
	case resp.StatusCode >= 500:
		return fmt.Errorf("backend %s: %s is not serving completions (HTTP %d): %s (retry later, or --celln-fleet-skip-preflight)", b.Name, b.Model.Endpoint, resp.StatusCode, strings.TrimSpace(string(snippet)))
	default:
		return fmt.Errorf("backend %s: %s answered HTTP %d to a minimal chat request: %s (check the model name and protocol)", b.Name, b.Model.Endpoint, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
}

// PreflightCredential is the key a probe uses: the named file, else the
// entry already published for the backend, else the keyless placeholder.
func PreflightCredential(ctx context.Context, store client.Client, b FleetBackend) (string, error) {
	if b.CredentialFile != "" {
		return readBackendCredential(b.CredentialFile)
	}
	if store != nil {
		var existing corev1.Secret
		err := store.Get(ctx, types.NamespacedName{Namespace: fleetNamespace, Name: FleetModelCredentialSecret}, &existing)
		if err != nil && !apierrors.IsNotFound(err) {
			return "", err
		}
		if err == nil && len(existing.Data[b.Name]) != 0 {
			return string(existing.Data[b.Name]), nil
		}
	}
	if !b.Model.NeedsCredential() {
		return fleetModelPlaceholderCredential, nil
	}
	return "", fmt.Errorf("backend %s needs a key: pass credential-file=/path (or --celln-fleet-model-credential-file for the native backend)", b.Name)
}
