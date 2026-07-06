package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// artifactMeta is the provenance + descriptive record persisted alongside
// each stored blob as "<id>.meta.json". Owner fields are derived from the
// authenticated TokenReview identity at upload time — they are trustworthy,
// not self-asserted by the client.
type artifactMeta struct {
	ID             string    `json:"id"`
	Filename       string    `json:"filename"`
	MimeType       string    `json:"mimeType"`
	Size           int64     `json:"size"`
	OwnerNamespace string    `json:"ownerNamespace"`
	OwnerSA        string    `json:"ownerServiceAccount"`
	CreatedAt      time.Time `json:"createdAt"`
	ExpiresAt      time.Time `json:"expiresAt"`
}

// expired reports whether the artifact is past its TTL as of now.
func (m artifactMeta) expired(now time.Time) bool {
	return !m.ExpiresAt.IsZero() && now.After(m.ExpiresAt)
}

// errNotFound is returned when an artifact id has no backing blob.
var errNotFound = errors.New("artifact not found")

// idPattern matches the 32-hex-char ids this server mints. Validating ids
// against it before touching the filesystem prevents path traversal: an id
// can never contain "/", "..", or a path separator.
var idPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// store is a filesystem-backed blob store. Each artifact is two files under
// DataDir: "<id>" (raw bytes) and "<id>.meta.json" (metadata). No database.
type store struct {
	dir string
}

func newStore(dir string) (*store, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	return &store{dir: dir}, nil
}

// newID returns a 128-bit random, unguessable identifier. Because it is
// infeasible to guess, possession of the id acts as a read capability on top
// of authentication and ownership authorization.
func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func validID(id string) bool {
	return idPattern.MatchString(id)
}

func (s *store) blobPath(id string) string {
	return filepath.Join(s.dir, id)
}

func (s *store) metaPath(id string) string {
	return filepath.Join(s.dir, id+".meta.json")
}

// put writes data and its metadata to disk and returns the completed record.
func (s *store) put(data []byte, meta artifactMeta) (artifactMeta, error) {
	id, err := newID()
	if err != nil {
		return artifactMeta{}, err
	}
	meta.ID = id
	meta.Size = int64(len(data))

	// Write the blob first, then the metadata. A crash between the two leaves
	// an orphan blob with no meta; the sweeper treats meta-less blobs as
	// garbage and removes them, so this ordering never yields a readable
	// artifact without provenance.
	if err := os.WriteFile(s.blobPath(id), data, 0o640); err != nil {
		return artifactMeta{}, fmt.Errorf("write blob: %w", err)
	}
	mb, err := json.Marshal(meta)
	if err != nil {
		_ = os.Remove(s.blobPath(id))
		return artifactMeta{}, err
	}
	if err := os.WriteFile(s.metaPath(id), mb, 0o640); err != nil {
		_ = os.Remove(s.blobPath(id))
		return artifactMeta{}, fmt.Errorf("write meta: %w", err)
	}
	return meta, nil
}

// getMeta reads the metadata record for an id.
func (s *store) getMeta(id string) (artifactMeta, error) {
	if !validID(id) {
		return artifactMeta{}, errNotFound
	}
	mb, err := os.ReadFile(s.metaPath(id))
	if err != nil {
		if os.IsNotExist(err) {
			return artifactMeta{}, errNotFound
		}
		return artifactMeta{}, err
	}
	var m artifactMeta
	if err := json.Unmarshal(mb, &m); err != nil {
		return artifactMeta{}, err
	}
	return m, nil
}

// open returns a read handle to the blob bytes. The caller must Close it.
func (s *store) open(id string) (*os.File, error) {
	if !validID(id) {
		return nil, errNotFound
	}
	f, err := os.Open(s.blobPath(id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errNotFound
		}
		return nil, err
	}
	return f, nil
}

// remove deletes both the blob and its metadata. Missing files are not an
// error so delete-on-delivery and the sweeper can race harmlessly.
func (s *store) remove(id string) error {
	if !validID(id) {
		return errNotFound
	}
	berr := os.Remove(s.blobPath(id))
	merr := os.Remove(s.metaPath(id))
	if berr != nil && !os.IsNotExist(berr) {
		return berr
	}
	if merr != nil && !os.IsNotExist(merr) {
		return merr
	}
	return nil
}

// pruneExpired removes artifacts past their TTL and orphan blobs whose
// metadata is missing. Returns the number of artifacts deleted.
func (s *store) pruneExpired(now time.Time) (int, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0, err
	}
	deleted := 0
	for _, e := range entries {
		name := e.Name()
		if !validID(name) { // only iterate blob files; skip *.meta.json
			continue
		}
		meta, err := s.getMeta(name)
		if err != nil {
			// Orphan blob (no/broken metadata) — remove it.
			if errors.Is(err, errNotFound) {
				_ = s.remove(name)
				deleted++
			}
			continue
		}
		if meta.expired(now) {
			if err := s.remove(name); err == nil {
				deleted++
			}
		}
	}
	return deleted, nil
}

// prunerLoop periodically sweeps expired/orphan artifacts.
func (s *store) prunerLoop(ctx context.Context, ttl time.Duration) {
	interval := ttl / 4
	if interval < time.Minute {
		interval = time.Minute
	}
	if interval > time.Hour {
		interval = time.Hour
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := s.pruneExpired(time.Now())
			if err != nil {
				slog.Warn("prune failed", "err", err)
				continue
			}
			if n > 0 {
				slog.Info("pruned expired artifacts", "deleted", n)
			}
		}
	}
}
