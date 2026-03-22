package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func stubHTTPGet(t *testing.T, fn func(string) (*http.Response, error)) {
	orig := updateHTTPGet
	t.Cleanup(func() { updateHTTPGet = orig })
	updateHTTPGet = fn
}

func stubCacheDir(t *testing.T, dir string) {
	orig := updateCacheDir
	t.Cleanup(func() { updateCacheDir = orig })
	updateCacheDir = func() (string, error) { return dir, nil }
}

func stubNow(t *testing.T, t0 time.Time) {
	orig := updateNow
	t.Cleanup(func() { updateNow = orig })
	updateNow = func() time.Time { return t0 }
}

func fakeHTTPResponse(statusCode int, body string) *http.Response {
	return &http.Response{
		StatusCode: statusCode,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestParseSemver(t *testing.T) {
	tests := []struct {
		input               string
		major, minor, patch int
		ok                  bool
	}{
		{"v1.2.3", 1, 2, 3, true},
		{"1.2.3", 1, 2, 3, true},
		{"v0.0.1", 0, 0, 1, true},
		{"v10.20.30", 10, 20, 30, true},
		{"invalid", 0, 0, 0, false},
		{"v1.2", 0, 0, 0, false},
		{"v1.2.3.4", 0, 0, 0, false},
		{"v1.2.beta", 0, 0, 0, false},
		{"", 0, 0, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			maj, min, pat, ok := parseSemver(tt.input)
			if maj != tt.major || min != tt.minor || pat != tt.patch || ok != tt.ok {
				t.Errorf("parseSemver(%q) = (%d,%d,%d,%v), want (%d,%d,%d,%v)",
					tt.input, maj, min, pat, ok, tt.major, tt.minor, tt.patch, tt.ok)
			}
		})
	}
}

func TestIsNewerVersion(t *testing.T) {
	tests := []struct {
		latest, current string
		want            bool
	}{
		{"v2.0.0", "v1.0.0", true},
		{"v1.1.0", "v1.0.0", true},
		{"v1.0.1", "v1.0.0", true},
		{"v1.0.0", "v1.0.0", false},
		{"v1.0.0", "v2.0.0", false},
		{"v1.0.0", "v1.1.0", false},
		{"v1.0.0", "v1.0.1", false},
		{"invalid", "v1.0.0", false},
		{"v1.0.0", "invalid", false},
		{"v2.0.0", "dev", false},
	}
	for _, tt := range tests {
		t.Run(tt.latest+"_vs_"+tt.current, func(t *testing.T) {
			got := isNewerVersion(tt.latest, tt.current)
			if got != tt.want {
				t.Errorf("isNewerVersion(%q, %q) = %v, want %v", tt.latest, tt.current, got, tt.want)
			}
		})
	}
}

func TestLoadUpdateCache_Missing(t *testing.T) {
	stubCacheDir(t, t.TempDir())
	cache := loadUpdateCache()
	if cache != nil {
		t.Errorf("expected nil for missing cache, got %+v", cache)
	}
}

func TestLoadUpdateCache_Valid(t *testing.T) {
	dir := t.TempDir()
	stubCacheDir(t, dir)

	data, _ := json.Marshal(updateCheckCache{LastCheck: 1000, LatestVersion: "v2.0.0"})
	os.WriteFile(filepath.Join(dir, updateCheckCacheFile), data, 0600)

	cache := loadUpdateCache()
	if cache == nil {
		t.Fatal("expected non-nil cache")
	}
	if cache.LatestVersion != "v2.0.0" {
		t.Errorf("LatestVersion = %q, want %q", cache.LatestVersion, "v2.0.0")
	}
}

func TestLoadUpdateCache_Invalid(t *testing.T) {
	dir := t.TempDir()
	stubCacheDir(t, dir)
	os.WriteFile(filepath.Join(dir, updateCheckCacheFile), []byte("not json"), 0600)

	cache := loadUpdateCache()
	if cache != nil {
		t.Errorf("expected nil for invalid JSON, got %+v", cache)
	}
}

func TestSaveUpdateCache(t *testing.T) {
	dir := t.TempDir()
	stubCacheDir(t, dir)

	saveUpdateCache(&updateCheckCache{LastCheck: 999, LatestVersion: "v3.0.0"})

	data, err := os.ReadFile(filepath.Join(dir, updateCheckCacheFile))
	if err != nil {
		t.Fatalf("cache file not written: %v", err)
	}
	var cache updateCheckCache
	if err := json.Unmarshal(data, &cache); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if cache.LatestVersion != "v3.0.0" || cache.LastCheck != 999 {
		t.Errorf("cache = %+v, want {999 v3.0.0}", cache)
	}
}

func TestCheckForUpdate_FreshCache(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(2000, 0)
	stubCacheDir(t, dir)
	stubNow(t, now)

	// Cache written 1 hour ago with a newer version.
	data, _ := json.Marshal(updateCheckCache{
		LastCheck:     now.Add(-1 * time.Hour).Unix(),
		LatestVersion: "v2.0.0",
	})
	os.WriteFile(filepath.Join(dir, updateCheckCacheFile), data, 0600)

	httpCalled := false
	stubHTTPGet(t, func(string) (*http.Response, error) {
		httpCalled = true
		return nil, nil
	})

	result := checkForUpdate("v1.0.0")
	if httpCalled {
		t.Error("HTTP should not be called when cache is fresh")
	}
	if result.LatestVersion != "v2.0.0" {
		t.Errorf("LatestVersion = %q, want %q", result.LatestVersion, "v2.0.0")
	}
}

func TestCheckForUpdate_FreshCacheSameVersion(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(2000, 0)
	stubCacheDir(t, dir)
	stubNow(t, now)

	data, _ := json.Marshal(updateCheckCache{
		LastCheck:     now.Add(-1 * time.Hour).Unix(),
		LatestVersion: "v1.0.0",
	})
	os.WriteFile(filepath.Join(dir, updateCheckCacheFile), data, 0600)

	result := checkForUpdate("v1.0.0")
	if result.LatestVersion != "" {
		t.Errorf("expected empty LatestVersion for same version, got %q", result.LatestVersion)
	}
}

func TestCheckForUpdate_StaleCache(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(200000, 0)
	stubCacheDir(t, dir)
	stubNow(t, now)

	// Cache written 25 hours ago.
	data, _ := json.Marshal(updateCheckCache{
		LastCheck:     now.Add(-25 * time.Hour).Unix(),
		LatestVersion: "v1.0.0",
	})
	os.WriteFile(filepath.Join(dir, updateCheckCacheFile), data, 0600)

	stubHTTPGet(t, func(string) (*http.Response, error) {
		body, _ := json.Marshal(githubRelease{TagName: "v2.0.0"})
		return fakeHTTPResponse(200, string(body)), nil
	})

	result := checkForUpdate("v1.0.0")
	if result.LatestVersion != "v2.0.0" {
		t.Errorf("LatestVersion = %q, want %q", result.LatestVersion, "v2.0.0")
	}
}

func TestCheckForUpdate_NoCache(t *testing.T) {
	dir := t.TempDir()
	stubCacheDir(t, dir)
	stubNow(t, time.Unix(1000, 0))

	stubHTTPGet(t, func(string) (*http.Response, error) {
		body, _ := json.Marshal(githubRelease{TagName: "v3.0.0"})
		return fakeHTTPResponse(200, string(body)), nil
	})

	result := checkForUpdate("v2.0.0")
	if result.LatestVersion != "v3.0.0" {
		t.Errorf("LatestVersion = %q, want %q", result.LatestVersion, "v3.0.0")
	}

	// Verify cache was written.
	cache := loadUpdateCache()
	if cache == nil || cache.LatestVersion != "v3.0.0" {
		t.Error("cache should be written after fetch")
	}
}

func TestCheckForUpdate_HTTPError(t *testing.T) {
	stubCacheDir(t, t.TempDir())
	stubNow(t, time.Unix(1000, 0))

	stubHTTPGet(t, func(string) (*http.Response, error) {
		return fakeHTTPResponse(500, ""), nil
	})

	result := checkForUpdate("v1.0.0")
	if result.Err == nil {
		t.Error("expected error for HTTP 500")
	}
	if result.LatestVersion != "" {
		t.Errorf("LatestVersion should be empty on error, got %q", result.LatestVersion)
	}
}

func TestCheckForUpdate_SameVersion(t *testing.T) {
	stubCacheDir(t, t.TempDir())
	stubNow(t, time.Unix(1000, 0))

	stubHTTPGet(t, func(string) (*http.Response, error) {
		body, _ := json.Marshal(githubRelease{TagName: "v1.0.0"})
		return fakeHTTPResponse(200, string(body)), nil
	})

	result := checkForUpdate("v1.0.0")
	if result.LatestVersion != "" {
		t.Errorf("expected empty LatestVersion for same version, got %q", result.LatestVersion)
	}
}

func TestCheckForUpdate_CurrentAhead(t *testing.T) {
	stubCacheDir(t, t.TempDir())
	stubNow(t, time.Unix(1000, 0))

	stubHTTPGet(t, func(string) (*http.Response, error) {
		body, _ := json.Marshal(githubRelease{TagName: "v1.0.0"})
		return fakeHTTPResponse(200, string(body)), nil
	})

	result := checkForUpdate("v2.0.0")
	if result.LatestVersion != "" {
		t.Errorf("expected empty LatestVersion when current is ahead, got %q", result.LatestVersion)
	}
}

func TestPrintUpdateNotification_Available(t *testing.T) {
	var buf bytes.Buffer
	printUpdateNotification(&buf, updateCheckResult{LatestVersion: "v2.0.0"}, "v1.0.0")
	out := buf.String()
	if !strings.Contains(out, "v2.0.0") {
		t.Errorf("output should contain latest version, got %q", out)
	}
	if !strings.Contains(out, "v1.0.0") {
		t.Errorf("output should contain current version, got %q", out)
	}
	if !strings.Contains(out, "brew upgrade") {
		t.Errorf("output should contain upgrade instructions, got %q", out)
	}
}

func TestPrintUpdateNotification_NotAvailable(t *testing.T) {
	var buf bytes.Buffer
	printUpdateNotification(&buf, updateCheckResult{}, "v1.0.0")
	if buf.Len() != 0 {
		t.Errorf("expected no output when no update, got %q", buf.String())
	}
}
