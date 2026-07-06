package main

import (
	"testing"
	"time"
)

func TestStorePutGetRoundTrip(t *testing.T) {
	st, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	meta := artifactMeta{
		Filename:       "chart.png",
		MimeType:       "image/png",
		OwnerNamespace: "team-x",
		OwnerSA:        "analyst-agent",
		CreatedAt:      now,
		ExpiresAt:      now.Add(time.Hour),
	}
	saved, err := st.put([]byte("PNGDATA"), meta)
	if err != nil {
		t.Fatal(err)
	}
	if !validID(saved.ID) {
		t.Fatalf("minted id %q is not valid", saved.ID)
	}
	if saved.Size != int64(len("PNGDATA")) {
		t.Fatalf("size = %d, want %d", saved.Size, len("PNGDATA"))
	}

	got, err := st.getMeta(saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.OwnerSA != "analyst-agent" || got.Filename != "chart.png" {
		t.Fatalf("meta round-trip mismatch: %+v", got)
	}

	f, err := st.open(saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	buf := make([]byte, 16)
	n, _ := f.Read(buf)
	if string(buf[:n]) != "PNGDATA" {
		t.Fatalf("blob bytes = %q, want PNGDATA", string(buf[:n]))
	}
}

func TestStoreRejectsTraversalIDs(t *testing.T) {
	st, _ := newStore(t.TempDir())
	for _, bad := range []string{"../etc/passwd", "abc/def", "..", "", "ABCDEF", "z0z0z0z0z0z0z0z0z0z0z0z0z0z0z0z0"} {
		if _, err := st.getMeta(bad); err != errNotFound {
			t.Errorf("getMeta(%q) err = %v, want errNotFound", bad, err)
		}
		if _, err := st.open(bad); err != errNotFound {
			t.Errorf("open(%q) err = %v, want errNotFound", bad, err)
		}
	}
}

func TestStorePruneExpiredAndOrphan(t *testing.T) {
	dir := t.TempDir()
	st, _ := newStore(dir)
	now := time.Now()

	live, _ := st.put([]byte("live"), artifactMeta{CreatedAt: now, ExpiresAt: now.Add(time.Hour)})
	dead, _ := st.put([]byte("dead"), artifactMeta{CreatedAt: now, ExpiresAt: now.Add(-time.Hour)})

	deleted, err := st.pruneExpired(now)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("pruned %d, want 1", deleted)
	}
	if _, err := st.getMeta(dead.ID); err != errNotFound {
		t.Fatalf("expired artifact still present")
	}
	if _, err := st.getMeta(live.ID); err != nil {
		t.Fatalf("live artifact was pruned: %v", err)
	}
}

func TestAuthorizeRead(t *testing.T) {
	cfg := &Config{
		AgentSASuffix:         "-agent",
		ChannelSASuffix:       "-channel",
		ReaderServiceAccounts: map[string]struct{}{"ops/deliver": {}},
		AdminServiceAccounts:  map[string]struct{}{"sys/admin": {}},
	}
	meta := artifactMeta{OwnerNamespace: "team-x", OwnerSA: "analyst-agent"}

	cases := []struct {
		name string
		id   identity
		want bool
	}{
		{"owner", identity{Namespace: "team-x", ServiceAccountName: "analyst-agent"}, true},
		{"sibling channel", identity{Namespace: "team-x", ServiceAccountName: "analyst-channel"}, true},
		{"different agent channel", identity{Namespace: "team-x", ServiceAccountName: "other-channel"}, false},
		{"channel wrong namespace", identity{Namespace: "team-y", ServiceAccountName: "analyst-channel"}, false},
		{"unrelated pod", identity{Namespace: "team-x", ServiceAccountName: "random-agent"}, false},
		{"allowlisted reader", identity{Namespace: "ops", ServiceAccountName: "deliver"}, true},
		{"admin flag", identity{Namespace: "who", ServiceAccountName: "ever", IsAdmin: true}, true},
		{"admin set", identity{Namespace: "sys", ServiceAccountName: "admin"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := authorizeRead(c.id, meta, cfg); got != c.want {
				t.Fatalf("authorizeRead(%s) = %v, want %v", c.id, got, c.want)
			}
		})
	}
}

func TestSanitizeFilename(t *testing.T) {
	cases := map[string]string{
		"chart.png":        "chart.png",
		"../../etc/passwd": "passwd",
		"/abs/path/x.csv":  "x.csv",
		"a\"b\nc.txt":      "abc.txt",
		"   ":              "",
		"..":               "",
	}
	for in, want := range cases {
		if got := sanitizeFilename(in); got != want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}
