package updatecheck

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	cacheTTL    = 24 * time.Hour
	httpTimeout = 5 * time.Second
	apiURL      = "https://api.github.com/repos/contenox/beam/releases/latest"
)

type cacheEntry struct {
	LatestTag string    `json:"latest_tag"`
	CheckedAt time.Time `json:"checked_at"`
}

// IsAvailable checks whether a newer version of contenox is available.
// It caches results for 24 h in contenoxDir/update-check.json to avoid
// hammering the GitHub API on every invocation.
// Returns the latest tag, whether it is newer than currentVersion, and any error.
func IsAvailable(ctx context.Context, currentVersion, contenoxDir string) (latestTag string, available bool, err error) {
	cachePath := filepath.Join(contenoxDir, "update-check.json")
	latestTag = readCache(cachePath)
	if latestTag == "" {
		latestTag, err = fetchFromGitHub(ctx)
		if err != nil {
			return "", false, err
		}
		writeCache(cachePath, latestTag)
	}
	return latestTag, isNewer(latestTag, currentVersion), nil
}

func readCache(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var e cacheEntry
	if err := json.Unmarshal(data, &e); err != nil || e.LatestTag == "" {
		return ""
	}
	if time.Since(e.CheckedAt) > cacheTTL {
		return ""
	}
	return e.LatestTag
}

func writeCache(path, tag string) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	data, err := json.Marshal(cacheEntry{LatestTag: tag, CheckedAt: time.Now()})
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o644)
}

func fetchFromGitHub(ctx context.Context) (string, error) {
	tctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(tctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "contenox-selfupdate")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub API returned status %d", resp.StatusCode)
	}

	var payload struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&payload); err != nil {
		return "", err
	}
	return strings.TrimSpace(payload.TagName), nil
}

// isNewer reports whether latest is a semver string strictly greater than current.
func isNewer(latest, current string) bool {
	l := strings.TrimPrefix(strings.TrimSpace(latest), "v")
	c := strings.TrimPrefix(strings.TrimSpace(current), "v")
	if l == "" || c == "" || l == c {
		return false
	}
	return semverGT(l, c)
}

func semverGT(a, b string) bool {
	ap := versionParts(a)
	bp := versionParts(b)
	for i := range ap {
		if ap[i] != bp[i] {
			return ap[i] > bp[i]
		}
	}
	return false
}

func versionParts(v string) [3]int {
	var parts [3]int
	segs := strings.SplitN(v, ".", 3)
	for i, s := range segs {
		if i >= 3 {
			break
		}
		_, _ = fmt.Sscanf(s, "%d", &parts[i])
	}
	return parts
}
