package main

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

// server bundles the HTTP handlers with their dependencies.
type server struct {
	cfg    *Config
	store  *store
	authFn authenticateFunc
	now    func() time.Time // injectable clock for tests
}

func newServer(cfg *Config, st *store, authFn authenticateFunc) *server {
	return &server{cfg: cfg, store: st, authFn: authFn, now: time.Now}
}

// handler builds the full HTTP mux: unauthenticated health probes plus the
// authenticated /v1 surface.
func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.healthz)

	protected := http.NewServeMux()
	protected.HandleFunc("POST /v1/artifacts", s.handleUpload)
	protected.HandleFunc("GET /v1/artifacts/{id}", s.handleDownload)
	protected.HandleFunc("DELETE /v1/artifacts/{id}", s.handleDelete)
	mux.Handle("/v1/", authMiddleware(s.authFn, protected))
	return mux
}

func (s *server) healthz(w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write([]byte("ok"))
}

// uploadResponse is the JSON returned to a successful producer.
type uploadResponse struct {
	ID        string    `json:"id"`
	Filename  string    `json:"filename"`
	MimeType  string    `json:"mimeType"`
	Size      int64     `json:"size"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// handleUpload stores a blob. The uploader identity comes from the
// authenticated token; the client cannot assert ownership.
func (s *server) handleUpload(w http.ResponseWriter, r *http.Request) {
	id, ok := identityFromCtx(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "no identity")
		return
	}

	// Cap the body. MaxBytesReader makes an over-limit upload fail at read
	// time rather than buffering the whole thing first.
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, "artifact exceeds max bytes or read failed")
		return
	}
	if len(data) == 0 {
		writeErr(w, http.StatusBadRequest, "empty body")
		return
	}

	filename := sanitizeFilename(r.Header.Get("X-Artifact-Filename"))
	mimeType := strings.TrimSpace(r.Header.Get("Content-Type"))
	if mimeType == "" || mimeType == "application/octet-stream" {
		if guessed := mime.TypeByExtension(filepath.Ext(filename)); guessed != "" {
			mimeType = guessed
		} else {
			mimeType = "application/octet-stream"
		}
	}

	ttl := s.cfg.TTL
	if h := r.Header.Get("X-Artifact-TTL"); h != "" {
		if d, perr := time.ParseDuration(h); perr == nil && d > 0 && d < ttl {
			ttl = d // callers may request a shorter, never a longer, life
		}
	}

	now := s.now()
	meta := artifactMeta{
		Filename:       filename,
		MimeType:       mimeType,
		OwnerNamespace: id.Namespace,
		OwnerSA:        id.ServiceAccountName,
		CreatedAt:      now,
		ExpiresAt:      now.Add(ttl),
	}
	saved, err := s.store.put(data, meta)
	if err != nil {
		slog.Error("store artifact", "err", err, "owner", id.String())
		writeErr(w, http.StatusInternalServerError, "store failed")
		return
	}
	slog.Info("artifact stored", "id", saved.ID, "owner", id.String(), "size", saved.Size, "filename", saved.Filename)
	writeJSON(w, http.StatusCreated, uploadResponse{
		ID:        saved.ID,
		Filename:  saved.Filename,
		MimeType:  saved.MimeType,
		Size:      saved.Size,
		ExpiresAt: saved.ExpiresAt,
	})
}

// handleDownload streams a blob after enforcing the three read gates:
// authentication (middleware), capability (unguessable id), authorization
// (owner, sibling channel SA, allowlist, or admin).
func (s *server) handleDownload(w http.ResponseWriter, r *http.Request) {
	id, ok := identityFromCtx(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "no identity")
		return
	}
	artifactID := r.PathValue("id")

	meta, err := s.store.getMeta(artifactID)
	if err != nil {
		// Unknown or unguessable-miss: 404 without confirming existence.
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if meta.expired(s.now()) {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if !authorizeRead(id, meta, s.cfg) {
		slog.Warn("artifact read denied", "id", artifactID, "reader", id.String(), "owner", meta.OwnerNamespace+"/"+meta.OwnerSA)
		writeErr(w, http.StatusForbidden, "forbidden")
		return
	}

	f, err := s.store.open(artifactID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	defer func() { _ = f.Close() }()

	w.Header().Set("Content-Type", meta.MimeType)
	if meta.Filename != "" {
		w.Header().Set("Content-Disposition", "attachment; filename=\""+meta.Filename+"\"")
	}
	w.Header().Set("X-Artifact-Owner", meta.OwnerNamespace+"/"+meta.OwnerSA)
	http.ServeContent(w, r, meta.Filename, meta.CreatedAt, f)
}

// handleDelete removes an artifact. Same authorization as reads so the
// delivering channel pod can clean up after a successful send.
func (s *server) handleDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := identityFromCtx(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "no identity")
		return
	}
	artifactID := r.PathValue("id")

	meta, err := s.store.getMeta(artifactID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if !authorizeRead(id, meta, s.cfg) {
		writeErr(w, http.StatusForbidden, "forbidden")
		return
	}
	if err := s.store.remove(artifactID); err != nil && !errors.Is(err, errNotFound) {
		writeErr(w, http.StatusInternalServerError, "delete failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// authorizeRead implements the convention-based authorization gate. It is a
// pure function of the caller identity, the artifact metadata, and config so
// it is trivially unit-testable without a cluster.
//
// A caller may read/delete an artifact when any of the following hold:
//   - admin: the identity is in ARTIFACT_ADMIN_SAS.
//   - allowlist: the identity is in ARTIFACT_READER_SERVICE_ACCOUNTS.
//   - owner: same namespace and service account that uploaded it.
//   - sibling channel: same namespace, and the caller's SA is the owning
//     agent's paired channel SA — i.e. stripping the channel suffix from the
//     caller and the agent suffix from the owner yields the same base name.
func authorizeRead(id identity, meta artifactMeta, cfg *Config) bool {
	if id.IsAdmin {
		return true
	}
	if _, ok := cfg.AdminServiceAccounts[id.String()]; ok {
		return true
	}
	if _, ok := cfg.ReaderServiceAccounts[id.String()]; ok {
		return true
	}
	// Owner.
	if id.Namespace == meta.OwnerNamespace && id.ServiceAccountName == meta.OwnerSA {
		return true
	}
	// Sibling channel pod of the owning agent.
	if id.Namespace == meta.OwnerNamespace &&
		strings.HasSuffix(id.ServiceAccountName, cfg.ChannelSASuffix) &&
		strings.HasSuffix(meta.OwnerSA, cfg.AgentSASuffix) {
		readerBase := strings.TrimSuffix(id.ServiceAccountName, cfg.ChannelSASuffix)
		ownerBase := strings.TrimSuffix(meta.OwnerSA, cfg.AgentSASuffix)
		if readerBase != "" && readerBase == ownerBase {
			return true
		}
	}
	return false
}

// sanitizeFilename reduces a client-supplied filename to a safe base name so
// it can never influence a filesystem path or a response header injection.
func sanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	name = filepath.Base(filepath.Clean(name))
	name = strings.Map(func(r rune) rune {
		if r == '"' || r == '\n' || r == '\r' || r < 0x20 {
			return -1
		}
		return r
	}, name)
	if name == "." || name == ".." || name == "/" {
		return ""
	}
	return name
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
