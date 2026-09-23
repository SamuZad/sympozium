package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func releaseArchive(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fakeReleases serves /latest as a redirect to tag and the archive plus its
// checksum file for that tag.
func fakeReleases(t *testing.T, tag, asset string, archive []byte, sum string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest":
			http.Redirect(w, r, "/releases/tag/"+tag, http.StatusFound)
		case "/releases/download/" + tag + "/" + asset:
			_, _ = w.Write(archive)
		case "/releases/download/" + tag + "/" + asset + ".sha256":
			fmt.Fprintf(w, "%s  %s\n", sum, asset)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	old := releaseBaseURL
	releaseBaseURL = srv.URL + "/releases"
	t.Cleanup(func() { releaseBaseURL = old })
}

func TestLatestReleaseTagFollowsRedirect(t *testing.T) {
	fakeReleases(t, "v1.2.3", "a.tar.gz", nil, "")
	tag, err := latestReleaseTag(context.Background(), http.DefaultClient)
	if err != nil || tag != "v1.2.3" {
		t.Fatalf("latestReleaseTag = %q, %v; want v1.2.3", tag, err)
	}
}

func TestDownloadReleaseBinaryVerifiesChecksum(t *testing.T) {
	archive := releaseArchive(t, "sympozium", []byte("new-binary"))
	sum := fmt.Sprintf("%x", sha256.Sum256(archive))

	fakeReleases(t, "v1.2.3", "sympozium-linux-amd64.tar.gz", archive, sum)
	bin, err := downloadReleaseBinary(context.Background(), http.DefaultClient, "v1.2.3", "sympozium-linux-amd64.tar.gz")
	if err != nil || string(bin) != "new-binary" {
		t.Fatalf("download = %q, %v", bin, err)
	}

	fakeReleases(t, "v1.2.3", "sympozium-linux-amd64.tar.gz", archive, strings.Repeat("0", 64))
	if _, err := downloadReleaseBinary(context.Background(), http.DefaultClient, "v1.2.3", "sympozium-linux-amd64.tar.gz"); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("tampered archive: err = %v, want checksum mismatch", err)
	}
}

func TestExtractBinaryRequiresTheBinary(t *testing.T) {
	if _, err := extractBinary(releaseArchive(t, "README.md", []byte("x")), "sympozium"); err == nil {
		t.Fatal("archive without the binary was accepted")
	}
}

func TestReplaceExecutableKeepsModeAndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "sympozium")
	if err := os.WriteFile(exe, []byte("old"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := replaceExecutable(exe, []byte("new")); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(exe)
	info, _ := os.Stat(exe)
	if string(got) != "new" || info.Mode().Perm() != 0o751 {
		t.Fatalf("content %q mode %v; want new 0751", got, info.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("staging file left behind: %v", entries)
	}
}
