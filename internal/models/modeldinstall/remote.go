package modeldinstall

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
)

// maxSumBytes caps the .sha256 read; the real file is well under 100 bytes.
const maxSumBytes = 64 * 1024

// httpStatusError carries a non-200 status for resources where the status has no
// dedicated sentinel.
type httpStatusError struct {
	status int
	url    string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("GET %s: HTTP %d", e.url, e.status)
}

func (c *client) userAgent() string {
	return fmt.Sprintf("contenox/%s modeld-setup", c.clientVersion)
}

// getSmallText fetches a small text resource (the .sha256 file). The index is
// the availability source of truth, so 403 and 404 on selected objects both map
// to artifact-unavailable fallback.
func (c *client) getSmallText(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", c.userAgent())
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		// proceed
	case http.StatusForbidden, http.StatusNotFound:
		return "", ErrArtifactUnavailable
	default:
		return "", &httpStatusError{status: resp.StatusCode, url: url}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxSumBytes))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// downloadToTemp streams the archive to a temp file created under dir and returns
// its path. The body is streamed to disk, never buffered in memory. On any error
// the temp file is removed. The caller verifies the checksum before using it.
func (c *client) downloadToTemp(ctx context.Context, url, dir, pattern string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", c.userAgent())
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
			return "", ErrArtifactUnavailable
		}
		return "", &httpStatusError{status: resp.StatusCode, url: url}
	}

	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	if err := streamWithProgress(f, resp.Body, resp.ContentLength, c.progress); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return tmp, nil
}

// streamWithProgress copies src to dst with a coarse MB progress line. The
// progress writer is io.Discard outside interactive setup.
func streamWithProgress(dst io.Writer, src io.Reader, total int64, progress io.Writer) error {
	buf := make([]byte, 1<<20)
	var written int64
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return werr
			}
			written += int64(n)
			if total > 0 {
				fmt.Fprintf(progress, "\r  %d MB / %d MB", written>>20, total>>20)
			} else {
				fmt.Fprintf(progress, "\r  %d MB", written>>20)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	if written > 0 {
		fmt.Fprintln(progress)
	}
	return nil
}
