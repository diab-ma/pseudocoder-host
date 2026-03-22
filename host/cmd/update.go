package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	updateCheckCacheFile = "update-check.json"
	updateCheckInterval  = 24 * time.Hour
	updateCheckURL       = "https://api.github.com/repos/diab-ma/pseudocoder-host/releases/latest"
	updateCheckTimeout   = 5 * time.Second
)

// updateCheckCache is the on-disk JSON format for ~/.pseudocoder/update-check.json.
type updateCheckCache struct {
	LastCheck     int64  `json:"last_check"`
	LatestVersion string `json:"latest_version"`
}

// updateCheckResult is the in-memory result of an update check.
type updateCheckResult struct {
	LatestVersion string
	Err           error
}

// githubRelease is the minimal subset of GitHub's release API response.
type githubRelease struct {
	TagName string `json:"tag_name"`
}

// Testability seams (matches start.go pattern).
var (
	updateHTTPGet = func(url string) (*http.Response, error) {
		client := &http.Client{Timeout: updateCheckTimeout}
		return client.Get(url)
	}
	updateCacheDir = func() (string, error) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".pseudocoder"), nil
	}
	updateNow = func() time.Time { return time.Now() }
)

// parseSemver parses "v1.2.3" or "1.2.3" into (major, minor, patch, ok).
func parseSemver(s string) (int, int, int, bool) {
	s = strings.TrimPrefix(s, "v")
	parts := strings.SplitN(s, ".", 3)
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	patch, err3 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, 0, 0, false
	}
	return major, minor, patch, true
}

// isNewerVersion returns true if latest is strictly newer than current.
func isNewerVersion(latest, current string) bool {
	lMaj, lMin, lPat, lok := parseSemver(latest)
	cMaj, cMin, cPat, cok := parseSemver(current)
	if !lok || !cok {
		return false
	}
	if lMaj != cMaj {
		return lMaj > cMaj
	}
	if lMin != cMin {
		return lMin > cMin
	}
	return lPat > cPat
}

// loadUpdateCache reads the cached update check result. Returns nil if missing/invalid.
func loadUpdateCache() *updateCheckCache {
	dir, err := updateCacheDir()
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(dir, updateCheckCacheFile))
	if err != nil {
		return nil
	}
	var cache updateCheckCache
	if json.Unmarshal(data, &cache) != nil {
		return nil
	}
	return &cache
}

// saveUpdateCache writes the cache file. Errors are silently ignored (best-effort).
func saveUpdateCache(cache *updateCheckCache) {
	dir, err := updateCacheDir()
	if err != nil {
		return
	}
	data, err := json.Marshal(cache)
	if err != nil {
		return
	}
	_ = os.MkdirAll(dir, 0700)
	_ = os.WriteFile(filepath.Join(dir, updateCheckCacheFile), data, 0600)
}

// checkForUpdate checks if a newer version is available.
// It uses a 24h file cache to avoid hitting GitHub on every startup.
func checkForUpdate(currentVersion string) updateCheckResult {
	cache := loadUpdateCache()
	now := updateNow()
	if cache != nil && now.Unix()-cache.LastCheck < int64(updateCheckInterval.Seconds()) {
		if isNewerVersion(cache.LatestVersion, currentVersion) {
			return updateCheckResult{LatestVersion: cache.LatestVersion}
		}
		return updateCheckResult{}
	}

	resp, err := updateHTTPGet(updateCheckURL)
	if err != nil {
		return updateCheckResult{Err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return updateCheckResult{Err: fmt.Errorf("GitHub API returned %d", resp.StatusCode)}
	}

	var release githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return updateCheckResult{Err: err}
	}

	saveUpdateCache(&updateCheckCache{
		LastCheck:     now.Unix(),
		LatestVersion: release.TagName,
	})

	if isNewerVersion(release.TagName, currentVersion) {
		return updateCheckResult{LatestVersion: release.TagName}
	}
	return updateCheckResult{}
}

// printUpdateNotification prints a one-liner if a newer version is available.
func printUpdateNotification(w io.Writer, result updateCheckResult, currentVersion string) {
	if result.LatestVersion == "" {
		return
	}
	fmt.Fprintf(w, "  Update available: %s (current: %s)\n", result.LatestVersion, currentVersion)
	fmt.Fprintln(w, "  Run 'brew upgrade pseudocoder' or visit https://github.com/diab-ma/pseudocoder-host/releases")
	fmt.Fprintln(w)
}
