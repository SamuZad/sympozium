package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	sympoziumv1alpha1 "github.com/sympozium-ai/sympozium/api/v1alpha1"
	"github.com/sympozium-ai/sympozium/pkg/memoryclient"
)

// recordingMemoryServer is a tiny test double that records every POST it
// receives and replies with a synthetic Entry so the controller can update
// its idempotency annotation.
type recordingMemoryServer struct {
	mu       sync.Mutex
	requests []memoryclient.StoreRequest
	deletes  []memoryclient.DeleteByTagsRequest
	srv      *httptest.Server
}

func newRecordingMemoryServer(t *testing.T) *recordingMemoryServer {
	t.Helper()
	rec := &recordingMemoryServer{}
	rec.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/store":
			var req memoryclient.StoreRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			rec.mu.Lock()
			rec.requests = append(rec.requests, req)
			rec.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"row-` + req.Content + `"}`))
		case "/v1/admin/delete-by-tags":
			var req memoryclient.DeleteByTagsRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			rec.mu.Lock()
			rec.deletes = append(rec.deletes, req)
			rec.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"deleted":1}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(rec.srv.Close)
	return rec
}

func (r *recordingMemoryServer) snapshot() []memoryclient.StoreRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]memoryclient.StoreRequest, len(r.requests))
	copy(out, r.requests)
	return out
}

func (r *recordingMemoryServer) deleteSnapshot() []memoryclient.DeleteByTagsRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]memoryclient.DeleteByTagsRequest, len(r.deletes))
	copy(out, r.deletes)
	return out
}

func TestReconcileMemorySeeds_NoopWhenClientNil(t *testing.T) {
	r, _ := newEnsembleTestReconciler(t)
	r.MemoryClient = nil

	pack := &sympoziumv1alpha1.Ensemble{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
	}
	persona := &sympoziumv1alpha1.AgentConfigSpec{
		Name:   "alice",
		Memory: &sympoziumv1alpha1.AgentConfigMemory{Seeds: []string{"a", "b"}},
	}
	if err := r.reconcileMemorySeeds(context.Background(), logr.Discard(), pack, persona, "p-alice"); err != nil {
		t.Fatalf("reconcileMemorySeeds: %v", err)
	}
}

func TestReconcileMemorySeeds_NoopWhenNoSeeds(t *testing.T) {
	rec := newRecordingMemoryServer(t)

	r, _ := newEnsembleTestReconciler(t)
	r.MemoryClient = memoryclient.New(rec.srv.URL, memoryclient.WithTokenSource(memoryclient.StaticTokenSource("t")))

	pack := &sympoziumv1alpha1.Ensemble{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"}}
	persona := &sympoziumv1alpha1.AgentConfigSpec{Name: "alice"} // no Memory

	if err := r.reconcileMemorySeeds(context.Background(), logr.Discard(), pack, persona, "p-alice"); err != nil {
		t.Fatalf("reconcileMemorySeeds: %v", err)
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Errorf("expected zero POSTs, got %d", len(got))
	}
}

func TestReconcileMemorySeeds_PostsAndDedupes(t *testing.T) {
	rec := newRecordingMemoryServer(t)

	inst := &sympoziumv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "p-alice", Namespace: "ns"},
	}
	r, cl := newEnsembleTestReconciler(t, inst)
	r.MemoryClient = memoryclient.New(rec.srv.URL, memoryclient.WithTokenSource(memoryclient.StaticTokenSource("t")))

	pack := &sympoziumv1alpha1.Ensemble{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
	}
	persona := &sympoziumv1alpha1.AgentConfigSpec{
		Name:   "alice",
		Memory: &sympoziumv1alpha1.AgentConfigMemory{Seeds: []string{"first", "second", " ", "third"}},
	}

	// First reconcile: all three real seeds should be posted.
	if err := r.reconcileMemorySeeds(context.Background(), logr.Discard(), pack, persona, "p-alice"); err != nil {
		t.Fatalf("reconcileMemorySeeds: %v", err)
	}

	got := rec.snapshot()
	if len(got) != 3 {
		t.Fatalf("first pass posted %d, want 3", len(got))
	}
	for _, req := range got {
		if req.Scope != "agent" || req.AgentName != "p-alice" {
			t.Errorf("unexpected scope/agentName: %+v", req)
		}
		if req.TTLDays != 0 {
			t.Errorf("unexpected TTLDays = %d, want 0", req.TTLDays)
		}
		hasFixed := map[string]bool{}
		hasSeedHash := false
		for _, tag := range req.Tags {
			switch tag {
			case "seed", "ensemble:p", "persona:alice":
				hasFixed[tag] = true
			default:
				if strings.HasPrefix(tag, "seed-hash:") {
					hasSeedHash = true
				} else {
					t.Errorf("unexpected tag %q in %v", tag, req.Tags)
				}
			}
		}
		if len(hasFixed) != 3 || !hasSeedHash {
			t.Errorf("tags missing required entries: %v", req.Tags)
		}
	}

	// Annotation should now list 3 distinct seed hashes.
	updated := &sympoziumv1alpha1.Agent{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "p-alice", Namespace: "ns"}, updated); err != nil {
		t.Fatalf("get instance: %v", err)
	}
	anno := updated.Annotations["sympozium.ai/memory-seeds-applied"]
	if anno == "" {
		t.Fatal("expected seeded annotation to be set")
	}
	if n := len(strings.Split(anno, ",")); n != 3 {
		t.Errorf("annotation has %d hashes, want 3 (raw=%q)", n, anno)
	}

	// Second reconcile with the same seeds: zero new POSTs.
	before := len(rec.snapshot())
	if err := r.reconcileMemorySeeds(context.Background(), logr.Discard(), pack, persona, "p-alice"); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if after := len(rec.snapshot()); after != before {
		t.Errorf("second pass posted %d new requests, want 0", after-before)
	}

	// Third reconcile with one new seed: exactly one new POST.
	persona.Memory.Seeds = append(persona.Memory.Seeds, "fourth")
	if err := r.reconcileMemorySeeds(context.Background(), logr.Discard(), pack, persona, "p-alice"); err != nil {
		t.Fatalf("third pass: %v", err)
	}
	final := rec.snapshot()
	if len(final) != 4 {
		t.Fatalf("third pass total POSTs = %d, want 4", len(final))
	}
	if final[3].Content != "fourth" {
		t.Errorf("last POST content = %q, want %q", final[3].Content, "fourth")
	}
}

func TestReconcileMemorySeeds_PartialFailurePersistsProgress(t *testing.T) {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts >= 2 {
			http.Error(w, "boom", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"x"}`))
	}))
	t.Cleanup(srv.Close)

	inst := &sympoziumv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "p-alice", Namespace: "ns"},
	}
	r, cl := newEnsembleTestReconciler(t, inst)
	r.MemoryClient = memoryclient.New(srv.URL, memoryclient.WithTokenSource(memoryclient.StaticTokenSource("t")))

	pack := &sympoziumv1alpha1.Ensemble{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"}}
	persona := &sympoziumv1alpha1.AgentConfigSpec{
		Name:   "alice",
		Memory: &sympoziumv1alpha1.AgentConfigMemory{Seeds: []string{"a", "b", "c"}},
	}

	err := r.reconcileMemorySeeds(context.Background(), logr.Discard(), pack, persona, "p-alice")
	if err == nil {
		t.Fatal("expected error on partial failure")
	}

	// One seed should have been persisted before the second call failed.
	updated := &sympoziumv1alpha1.Agent{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "p-alice", Namespace: "ns"}, updated); err != nil {
		t.Fatalf("get instance: %v", err)
	}
	anno := updated.Annotations["sympozium.ai/memory-seeds-applied"]
	parts := strings.Split(anno, ",")
	if len(parts) != 1 {
		t.Errorf("annotation has %d hashes, want 1 (raw=%q)", len(parts), anno)
	}
}

func TestSeedHash_DeterministicAndDistinct(t *testing.T) {
	a := seedHash("hello")
	b := seedHash("hello")
	c := seedHash("world")
	if a != b {
		t.Errorf("hash of same input differs: %q vs %q", a, b)
	}
	if a == c {
		t.Errorf("hash collision: %q == %q", a, c)
	}
	if len(a) != 16 {
		t.Errorf("hash length = %d, want 16", len(a))
	}
}

func TestParseSeedHashSet(t *testing.T) {
	cases := map[string]int{
		"":              0,
		"abc":           1,
		"abc,def":       2,
		" abc , def ,":  2,
		"abc,abc,abc":   1,
	}
	for in, want := range cases {
		got := parseSeedHashSet(in)
		if len(got) != want {
			t.Errorf("parseSeedHashSet(%q) -> %d entries, want %d", in, len(got), want)
		}
	}
}

func TestReconcileMemorySeeds_AppliesSeedTTLDays(t *testing.T) {
	rec := newRecordingMemoryServer(t)

	inst := &sympoziumv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "p-alice", Namespace: "ns"},
	}
	r, _ := newEnsembleTestReconciler(t, inst)
	r.MemoryClient = memoryclient.New(rec.srv.URL, memoryclient.WithTokenSource(memoryclient.StaticTokenSource("t")))

	ttl := 7
	pack := &sympoziumv1alpha1.Ensemble{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"}}
	persona := &sympoziumv1alpha1.AgentConfigSpec{
		Name: "alice",
		Memory: &sympoziumv1alpha1.AgentConfigMemory{
			Seeds:       []string{"transient"},
			SeedTTLDays: &ttl,
		},
	}

	if err := r.reconcileMemorySeeds(context.Background(), logr.Discard(), pack, persona, "p-alice"); err != nil {
		t.Fatalf("reconcileMemorySeeds: %v", err)
	}
	got := rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("posted %d, want 1", len(got))
	}
	if got[0].TTLDays != 7 {
		t.Errorf("TTLDays = %d, want 7", got[0].TTLDays)
	}
}

func TestReconcileMemorySeeds_GarbageCollectsOrphans(t *testing.T) {
	rec := newRecordingMemoryServer(t)

	inst := &sympoziumv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "p-alice", Namespace: "ns"},
	}
	r, cl := newEnsembleTestReconciler(t, inst)
	r.MemoryClient = memoryclient.New(rec.srv.URL, memoryclient.WithTokenSource(memoryclient.StaticTokenSource("t")))

	pack := &sympoziumv1alpha1.Ensemble{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"}}
	persona := &sympoziumv1alpha1.AgentConfigSpec{
		Name:   "alice",
		Memory: &sympoziumv1alpha1.AgentConfigMemory{Seeds: []string{"keep", "drop-me"}},
	}

	if err := r.reconcileMemorySeeds(context.Background(), logr.Discard(), pack, persona, "p-alice"); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if got := len(rec.snapshot()); got != 2 {
		t.Fatalf("first pass posted %d, want 2", got)
	}
	if got := len(rec.deleteSnapshot()); got != 0 {
		t.Errorf("first pass deleted %d, want 0", got)
	}

	// Remove "drop-me" — GC should fire exactly one tag-filtered delete
	// scoped to the orphan's hash, and the annotation should shrink to 1.
	persona.Memory.Seeds = []string{"keep"}
	if err := r.reconcileMemorySeeds(context.Background(), logr.Discard(), pack, persona, "p-alice"); err != nil {
		t.Fatalf("second pass: %v", err)
	}

	deletes := rec.deleteSnapshot()
	if len(deletes) != 1 {
		t.Fatalf("GC issued %d deletes, want 1", len(deletes))
	}
	d := deletes[0]
	if d.Scope != "agent" || d.AgentName != "p-alice" || d.Namespace != "ns" {
		t.Errorf("delete scope mismatch: %+v", d)
	}
	wantHash := "seed-hash:" + seedHash("drop-me")
	foundHash := false
	for _, tag := range d.RequireTags {
		if tag == wantHash {
			foundHash = true
		}
	}
	if !foundHash {
		t.Errorf("expected %q in RequireTags, got %v", wantHash, d.RequireTags)
	}

	updated := &sympoziumv1alpha1.Agent{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "p-alice", Namespace: "ns"}, updated); err != nil {
		t.Fatalf("get instance: %v", err)
	}
	anno := updated.Annotations["sympozium.ai/memory-seeds-applied"]
	parts := strings.Split(anno, ",")
	if len(parts) != 1 || parts[0] != seedHash("keep") {
		t.Errorf("annotation = %q, want hash of 'keep' only", anno)
	}

	// Drop everything: GC should remove the last seed.
	persona.Memory.Seeds = nil
	if err := r.reconcileMemorySeeds(context.Background(), logr.Discard(), pack, persona, "p-alice"); err != nil {
		t.Fatalf("third pass: %v", err)
	}
	if got := len(rec.deleteSnapshot()); got != 2 {
		t.Errorf("third pass cumulative deletes = %d, want 2", got)
	}
}
