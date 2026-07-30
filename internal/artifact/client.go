// Package artifact provides the HTTP client agent pods use to exchange
// attachment bytes with the central artifact-server, so binaries never ride
// the event bus.
package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Client talks to the artifact-server using the pod's ServiceAccount token.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewClientFromEnv returns a configured client, or nil if ARTIFACT_SERVER_URL
// is unset or the pod has no ServiceAccount token.
func NewClientFromEnv() *Client {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("ARTIFACT_SERVER_URL")), "/")
	if base == "" {
		return nil
	}
	token, err := readServiceAccountToken()
	if err != nil || token == "" {
		fmt.Fprintf(os.Stderr, "artifact: client disabled (no SA token): %v\n", err)
		return nil
	}
	return &Client{
		baseURL: base,
		token:   token,
		http:    &http.Client{Timeout: 60 * time.Second},
	}
}

// Upload posts a single file and returns the artifact id and stored size.
func (c *Client) Upload(ctx context.Context, filename, mimeType string, data []byte) (string, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/artifacts", bytes.NewReader(data))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", mimeType)
	req.Header.Set("X-Artifact-Filename", filename)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusCreated {
		return "", 0, fmt.Errorf("artifact-server HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var ur struct {
		ID   string `json:"id"`
		Size int64  `json:"size"`
	}
	if err := json.Unmarshal(body, &ur); err != nil {
		return "", 0, err
	}
	if ur.ID == "" {
		return "", 0, fmt.Errorf("artifact-server returned empty id")
	}
	return ur.ID, ur.Size, nil
}

// Fetch downloads an artifact's bytes by id, returning the data and the
// original filename from the Content-Disposition header (may be empty).
func (c *Client) Fetch(ctx context.Context, id string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/artifacts/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return nil, "", fmt.Errorf("artifact-server HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	filename := ""
	if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil {
		filename = params["filename"]
	}
	return data, filename, nil
}

// readServiceAccountToken reads the projected pod SA token used to
// authenticate to the artifact-server. SA_TOKEN_PATH overrides the location
// (tests).
func readServiceAccountToken() (string, error) {
	path := os.Getenv("SA_TOKEN_PATH")
	if path == "" {
		path = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
