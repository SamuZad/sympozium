package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	authnv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// identity describes the caller derived from a Bearer token.
type identity struct {
	Namespace          string
	ServiceAccountName string
	UID                string
	IsAdmin            bool
}

// String returns the canonical "<ns>/<sa>" identity key.
func (i identity) String() string {
	return i.Namespace + "/" + i.ServiceAccountName
}

// authenticator validates SA tokens via the TokenReview API and caches
// results to keep the kube-apiserver request rate sane.
//
// The kube client is held behind a factory so the TokenReview call can
// rebuild it on Unauthorized — defensive against the projected SA token
// on disk being stale relative to what kubelet rotated. Under normal
// operation client-go re-reads the file every minute; this guards the
// rare case where reads and rotations race past expiry.
type authenticator struct {
	cache  *lru.Cache[string, cachedIdentity]
	ttl    time.Duration
	admins map[string]struct{}

	newClient func() (kubernetes.Interface, error)

	mu          sync.RWMutex
	k8s         kubernetes.Interface
	lastRefresh time.Time
}

type cachedIdentity struct {
	id        identity
	expiresAt time.Time
}

func newAuthenticator(newClient func() (kubernetes.Interface, error), cacheSize int, ttl time.Duration, admins map[string]struct{}) (*authenticator, error) {
	if cacheSize <= 0 {
		cacheSize = 1024
	}
	c, err := lru.New[string, cachedIdentity](cacheSize)
	if err != nil {
		return nil, err
	}
	k8s, err := newClient()
	if err != nil {
		return nil, err
	}
	return &authenticator{
		cache:       c,
		ttl:         ttl,
		admins:      admins,
		newClient:   newClient,
		k8s:         k8s,
		lastRefresh: time.Now(),
	}, nil
}

// client returns the current kube client.
func (a *authenticator) client() kubernetes.Interface {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.k8s
}

// rebuildClient replaces the kube client with a fresh one, forcing client-go
// to re-read the projected SA token from disk. It rate-limits rebuilds to at
// most once per 30s so a flood of bad tokens can't trigger thundering rebuilds.
func (a *authenticator) rebuildClient() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if time.Since(a.lastRefresh) < 30*time.Second {
		return nil
	}
	k8s, err := a.newClient()
	if err != nil {
		return err
	}
	a.k8s = k8s
	a.lastRefresh = time.Now()
	return nil
}

func newK8sClient() (kubernetes.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}
	return kubernetes.NewForConfig(cfg)
}

// authenticate validates a Bearer token and returns the caller's identity.
// Tokens are hashed (SHA-256) before being used as cache keys so the raw
// secret never lives on the LRU's heap layout in a recoverable form.
func (a *authenticator) authenticate(ctx context.Context, token string) (identity, error) {
	if token == "" {
		return identity{}, errors.New("missing bearer token")
	}
	sum := sha256.Sum256([]byte(token))
	key := hex.EncodeToString(sum[:])

	if v, ok := a.cache.Get(key); ok && time.Now().Before(v.expiresAt) {
		return v.id, nil
	}

	id, err := a.review(ctx, token)
	if err == nil {
		a.cache.Add(key, cachedIdentity{id: id, expiresAt: time.Now().Add(a.ttl)})
	}
	return id, err
}

func (a *authenticator) review(ctx context.Context, token string) (identity, error) {
	id, err := a.reviewOnce(ctx, token)
	if err == nil {
		return id, nil
	}
	// If kube-apiserver rejects *our own* SA token (401), the projected
	// token on disk is likely stale. Rebuild the client (which re-reads
	// the token file) and retry once.
	if !isUnauthorized(err) {
		return id, err
	}
	slog.Warn("tokenreview unauthorized; refreshing kube client", "err", err)
	if rerr := a.rebuildClient(); rerr != nil {
		slog.Error("rebuild kube client failed", "err", rerr)
		return id, err
	}
	return a.reviewOnce(ctx, token)
}

func (a *authenticator) reviewOnce(ctx context.Context, token string) (identity, error) {
	tr := &authnv1.TokenReview{
		Spec: authnv1.TokenReviewSpec{Token: token},
	}
	out, err := a.client().AuthenticationV1().TokenReviews().Create(ctx, tr, metav1.CreateOptions{})
	if err != nil {
		return identity{}, fmt.Errorf("tokenreview: %w", err)
	}
	if !out.Status.Authenticated {
		return identity{}, fmt.Errorf("token not authenticated: %s", out.Status.Error)
	}
	// User.Username for SA tokens has the form
	// "system:serviceaccount:<namespace>:<name>"
	u := out.Status.User.Username
	const prefix = "system:serviceaccount:"
	if !strings.HasPrefix(u, prefix) {
		return identity{}, fmt.Errorf("non-serviceaccount caller %q is not supported", u)
	}
	rest := strings.TrimPrefix(u, prefix)
	parts := strings.SplitN(rest, ":", 2)
	if len(parts) != 2 {
		return identity{}, fmt.Errorf("malformed sa username %q", u)
	}
	id := identity{
		Namespace:          parts[0],
		ServiceAccountName: parts[1],
		UID:                out.Status.User.UID,
	}
	if _, ok := a.admins[id.String()]; ok {
		id.IsAdmin = true
	}
	return id, nil
}

// isUnauthorized reports whether err represents a 401 from kube-apiserver.
// It checks both the structured StatusError code and the textual marker that
// client-go bubbles up when the bearer token is rejected before the request
// is dispatched ("the server has asked for the client to provide credentials").
func isUnauthorized(err error) bool {
	if err == nil {
		return false
	}
	if apierrors.IsUnauthorized(err) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "Unauthorized") ||
		strings.Contains(s, "the server has asked for the client to provide credentials")
}


// authMiddleware enforces bearer-token authentication on protected routes
// and stashes the resolved identity on the request context.
type ctxKey string

const ctxKeyIdentity ctxKey = "identity"

func (a *authenticator) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		id, err := a.authenticate(r.Context(), token)
		if err != nil {
			slog.Warn("auth failed", "err", err, "path", r.URL.Path)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), ctxKeyIdentity, id))
		next.ServeHTTP(w, r)
	})
}

func identityFromCtx(ctx context.Context) (identity, bool) {
	id, ok := ctx.Value(ctxKeyIdentity).(identity)
	return id, ok
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	const p = "Bearer "
	if !strings.HasPrefix(h, p) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(h, p))
}
