package modeldinstall

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/contenox/contenox/libtransport"
)

const maxIndexBytes = 4 * 1024 * 1024

type indexDocument struct {
	Schema int          `json:"schema"`
	Builds []indexBuild `json:"builds"`
}

type indexBuild struct {
	Version  string   `json:"version"`
	Platform string   `json:"platform"`
	Protocol int      `json:"protocol"`
	Backends []string `json:"backends"`
	Channel  string   `json:"channel"`
	Archive  string   `json:"archive"`
	SHA256   string   `json:"sha256"`
	Size     int64    `json:"size,omitempty"`
}

func (c *client) fetchIndex(ctx context.Context) (indexDocument, error) {
	url := objectURL(c.baseURL, "index.json")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return indexDocument{}, err
	}
	req.Header.Set("User-Agent", c.userAgent())
	resp, err := c.http.Do(req)
	if err != nil {
		return indexDocument{}, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusForbidden, http.StatusNotFound:
		return indexDocument{}, ErrNoIndex
	default:
		return indexDocument{}, &httpStatusError{status: resp.StatusCode, url: url}
	}
	var idx indexDocument
	dec := json.NewDecoder(io.LimitReader(resp.Body, maxIndexBytes))
	if err := dec.Decode(&idx); err != nil {
		return indexDocument{}, fmt.Errorf("modeld setup: parse release index: %w", err)
	}
	if idx.Schema != 1 {
		return indexDocument{}, fmt.Errorf("modeld setup: unsupported release index schema %d", idx.Schema)
	}
	return idx, nil
}

func selectBuild(idx indexDocument, platform, provider string) (indexBuild, error) {
	var best indexBuild
	found := false
	for _, b := range idx.Builds {
		if b.Platform != platform {
			continue
		}
		if b.Channel != "stable" {
			continue
		}
		if !transport.Supported(b.Protocol) {
			continue
		}
		if !containsString(b.Backends, provider) {
			continue
		}
		if !found || compareVersions(b.Version, best.Version) > 0 {
			best = b
			found = true
		}
	}
	if !found {
		return indexBuild{}, ErrNoCompatibleArtifact
	}
	return best, nil
}

func compareVersions(a, b string) int {
	av, aok := parseVersion(a)
	bv, bok := parseVersion(b)
	if aok && bok {
		for i := 0; i < len(av.nums) || i < len(bv.nums); i++ {
			an, bn := 0, 0
			if i < len(av.nums) {
				an = av.nums[i]
			}
			if i < len(bv.nums) {
				bn = bv.nums[i]
			}
			if an < bn {
				return -1
			}
			if an > bn {
				return 1
			}
		}
		if av.pre == "" && bv.pre != "" {
			return 1
		}
		if av.pre != "" && bv.pre == "" {
			return -1
		}
		if av.pre < bv.pre {
			return -1
		}
		if av.pre > bv.pre {
			return 1
		}
		return 0
	}
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

type parsedVersion struct {
	nums []int
	pre  string
}

func parseVersion(v string) (parsedVersion, bool) {
	v = strings.TrimSpace(strings.TrimPrefix(v, "v"))
	if v == "" {
		return parsedVersion{}, false
	}
	if i := strings.IndexByte(v, '+'); i >= 0 {
		v = v[:i]
	}
	pre := ""
	if i := strings.IndexByte(v, '-'); i >= 0 {
		pre = v[i+1:]
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	nums := make([]int, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			return parsedVersion{}, false
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return parsedVersion{}, false
		}
		nums = append(nums, n)
	}
	return parsedVersion{nums: nums, pre: pre}, true
}

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
