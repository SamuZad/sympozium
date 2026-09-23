package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// releaseBaseURL is where release pages and assets live; a variable so tests
// can point it at a local server.
var releaseBaseURL = "https://github.com/" + ghRepo + "/releases"

// maxReleaseArchiveBytes bounds a downloaded archive and the binary in it.
const maxReleaseArchiveBytes = 512 << 20

func newUpdateCmd() *cobra.Command {
	var targetTag string
	var checkOnly bool
	var force bool
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update this sympozium CLI to the latest release",
		Long: `Downloads the latest sympozium CLI release from GitHub for this platform,
verifies it against the release's published SHA-256 checksum and replaces the
running binary in place.

This updates the CLI only. Run 'sympozium upgrade' afterwards to move the
cluster's installation to the new release.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			client := &http.Client{Timeout: 5 * time.Minute}
			tag := targetTag
			if tag == "" {
				latest, err := latestReleaseTag(ctx, client)
				if err != nil {
					return err
				}
				tag = latest
			} else if !strings.HasPrefix(tag, "v") {
				tag = "v" + tag
			}
			fmt.Printf("  Current: %s\n  Target:  %s\n", version, tag)
			if tag == version && !force {
				fmt.Println("  Already up to date.")
				return nil
			}
			if checkOnly {
				fmt.Println("  An update is available; run 'sympozium update' to install it.")
				return nil
			}
			if isDevBuild() && !force {
				return fmt.Errorf("this is a source build (version %q); pass --force to replace it with release %s", version, tag)
			}

			exe, err := os.Executable()
			if err != nil {
				return fmt.Errorf("locate running binary: %w", err)
			}
			if exe, err = filepath.EvalSymlinks(exe); err != nil {
				return fmt.Errorf("resolve running binary: %w", err)
			}
			if strings.Contains(exe, "/Cellar/") {
				return fmt.Errorf("%s is managed by Homebrew; run 'brew upgrade sympozium' instead", exe)
			}

			asset := fmt.Sprintf("sympozium-%s-%s.tar.gz", runtime.GOOS, runtime.GOARCH)
			fmt.Printf("  Downloading %s...\n", asset)
			bin, err := downloadReleaseBinary(ctx, client, tag, asset)
			if err != nil {
				return err
			}
			if err := replaceExecutable(exe, bin); err != nil {
				if errors.Is(err, os.ErrPermission) {
					return fmt.Errorf("%w\n  Rerun with sufficient permissions (e.g. 'sudo sympozium update')", err)
				}
				return err
			}
			fmt.Printf("  Updated %s to %s.\n", exe, tag)
			fmt.Println("  Run 'sympozium upgrade' to upgrade the cluster installation to this release.")
			return nil
		},
	}
	cmd.Flags().StringVar(&targetTag, "version", "", "Install this release tag (e.g. v0.10.83) instead of the latest")
	cmd.Flags().BoolVar(&checkOnly, "check", false, "Only report whether an update is available")
	cmd.Flags().BoolVar(&force, "force", false, "Reinstall even when already on the target version, or replace a source build")
	return cmd
}

// latestReleaseTag reads the latest release tag from the releases/latest
// redirect rather than the API, which rate-limits unauthenticated callers.
func latestReleaseTag(ctx context.Context, client *http.Client) (string, error) {
	noRedirect := *client
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, releaseBaseURL+"/latest", nil)
	if err != nil {
		return "", err
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		return "", fmt.Errorf("query latest release: %w", err)
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	i := strings.LastIndex(loc, "/tag/")
	if resp.StatusCode/100 != 3 || i == -1 {
		return "", fmt.Errorf("could not determine the latest release (HTTP %d); check %s", resp.StatusCode, releaseBaseURL)
	}
	tag := loc[i+len("/tag/"):]
	if tag == "" || strings.ContainsAny(tag, "/?# ") {
		return "", fmt.Errorf("unexpected latest release location %q", loc)
	}
	return tag, nil
}

// downloadReleaseBinary fetches a release archive and its .sha256, verifies
// the archive and returns the sympozium binary inside it.
func downloadReleaseBinary(ctx context.Context, client *http.Client, tag, asset string) ([]byte, error) {
	base := releaseBaseURL + "/download/" + tag + "/" + asset
	archive, err := httpGetBounded(ctx, client, base)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", asset, err)
	}
	sumFile, err := httpGetBounded(ctx, client, base+".sha256")
	if err != nil {
		return nil, fmt.Errorf("download %s.sha256: %w", asset, err)
	}
	fields := strings.Fields(string(sumFile))
	if len(fields) == 0 {
		return nil, fmt.Errorf("%s.sha256 is empty", asset)
	}
	got := sha256.Sum256(archive)
	if !strings.EqualFold(fields[0], hex.EncodeToString(got[:])) {
		return nil, fmt.Errorf("checksum mismatch for %s: release says %s, downloaded %x", asset, fields[0], got)
	}
	return extractBinary(archive, "sympozium")
}

func httpGetBounded(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxReleaseArchiveBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxReleaseArchiveBytes {
		return nil, fmt.Errorf("GET %s: response exceeds %d bytes", url, maxReleaseArchiveBytes)
	}
	return data, nil
}

// extractBinary returns the regular file named name from a .tar.gz archive.
func extractBinary(archive []byte, name string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("read archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("archive has no %q binary", name)
		}
		if err != nil {
			return nil, fmt.Errorf("read archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg || filepath.Base(hdr.Name) != name {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, maxReleaseArchiveBytes+1))
		if err != nil {
			return nil, fmt.Errorf("read %s from archive: %w", name, err)
		}
		if len(data) > maxReleaseArchiveBytes {
			return nil, fmt.Errorf("%s in archive exceeds %d bytes", name, maxReleaseArchiveBytes)
		}
		return data, nil
	}
}

// replaceExecutable atomically swaps exe for bin: it writes a sibling temp
// file (same filesystem) and renames it over the original.
func replaceExecutable(exe string, bin []byte) error {
	mode := os.FileMode(0o755)
	if info, err := os.Stat(exe); err == nil {
		mode = info.Mode().Perm() | 0o111
	}
	tmp, err := os.CreateTemp(filepath.Dir(exe), ".sympozium-update-*")
	if err != nil {
		return fmt.Errorf("stage update next to %s: %w", exe, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(bin); err != nil {
		tmp.Close()
		return fmt.Errorf("write update: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write update: %w", err)
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return fmt.Errorf("chmod update: %w", err)
	}
	if err := os.Rename(tmpName, exe); err != nil {
		return fmt.Errorf("replace %s: %w", exe, err)
	}
	return nil
}
