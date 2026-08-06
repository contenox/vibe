package libacp

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

const (
	defaultReadBuf    = 64 * 1024
	maxNDJSONFrame    = 64 * 1024 * 1024
	maxWireDumpBytes  = 256 * 1024
	wireDumpTruncated = " [contenox wire log truncated"
)

var (
	wireMu  sync.Mutex
	wireOut io.Writer
)

func init() {
	if p := os.Getenv("CONTENOX_ACP_WIRE_LOG"); p != "" {
		if f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			wireOut = f
		}
	}
}

func wireDump(dir string, b []byte) {
	if wireOut == nil {
		return
	}
	originalLen := len(b)
	truncated := originalLen > maxWireDumpBytes
	if truncated {
		b = b[:maxWireDumpBytes]
	}
	wireMu.Lock()
	defer wireMu.Unlock()
	if truncated {
		fmt.Fprintf(wireOut, "%s %s %s%s original_bytes=%d]\n", time.Now().Format(time.RFC3339Nano), dir, b, wireDumpTruncated, originalLen)
		return
	}
	fmt.Fprintf(wireOut, "%s %s %s\n", time.Now().Format(time.RFC3339Nano), dir, b)
}

type ndjsonReader struct {
	reader *bufio.Reader
}

func newNDJSONReader(r io.Reader) *ndjsonReader {
	return &ndjsonReader{reader: bufio.NewReaderSize(r, defaultReadBuf)}
}

func (r *ndjsonReader) Next() ([]byte, error) {
	for {
		line, err := r.readLine()
		if err != nil {
			return nil, err
		}
		if len(line) == 0 {
			continue
		}
		wireDump("<-", line)
		return line, nil
	}
}

func (r *ndjsonReader) readLine() ([]byte, error) {
	var line []byte
	for {
		frag, err := r.reader.ReadSlice('\n')
		if len(frag) > 0 {
			if len(line)+len(frag) > maxNDJSONFrame {
				return nil, fmt.Errorf("libacp: ndjson frame too large: exceeds %d bytes", maxNDJSONFrame)
			}
			line = append(line, frag...)
		}
		switch {
		case err == nil:
			if len(line) > 0 && line[len(line)-1] == '\n' {
				line = line[:len(line)-1]
			}
			return line, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if len(line) == 0 {
				return nil, io.EOF
			}
			return line, nil
		default:
			return nil, err
		}
	}
}

type ndjsonWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func newNDJSONWriter(w io.Writer) *ndjsonWriter {
	return &ndjsonWriter{w: w}
}

func (w *ndjsonWriter) Write(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("libacp: marshal: %w", err)
	}
	wireDump("->", data)
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.w.Write(data); err != nil {
		return err
	}
	if _, err := w.w.Write([]byte{'\n'}); err != nil {
		return err
	}
	return nil
}
