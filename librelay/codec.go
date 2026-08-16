package librelay

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

const readBufSize = 64 * 1024

// Codec-level failures. ErrFrameTooLarge is the only one that ends a connection;
// every other error is per-frame and the [Reader] resumes at the next line.
var (
	ErrFrameTooLarge = errors.New("librelay: frame exceeds MaxFrameBytes")
	ErrReaderClosed  = errors.New("librelay: reader is unusable after a framing error")
)

// Reader decodes NDJSON frames from a stream. It is not safe for concurrent
// use; one goroutine owns the read side of a connection.
type Reader struct {
	br   *bufio.Reader
	dead error
}

// NewReader returns a Reader over r.
func NewReader(r io.Reader) *Reader {
	return &Reader{br: bufio.NewReaderSize(r, readBufSize)}
}

// ReadFrame returns the next frame. A malformed or invalid frame is reported
// with the line already consumed, so calling ReadFrame again reads the next one;
// after [ErrFrameTooLarge] the Reader is dead and every later call returns
// [ErrReaderClosed].
func (r *Reader) ReadFrame() (Frame, error) {
	if r.dead != nil {
		return Frame{}, r.dead
	}
	line, err := r.readLine()
	if err != nil {
		return Frame{}, err
	}
	var f Frame
	// Unknown fields are accepted deliberately: the envelope evolves by addition.
	if err := json.Unmarshal(line, &f); err != nil {
		return Frame{}, fmt.Errorf("librelay: decode frame: %w", err)
	}
	if err := f.Validate(); err != nil {
		return Frame{}, err
	}
	return f, nil
}

// readLine returns one newline-delimited line with blank lines skipped, refusing
// to accumulate past MaxFrameBytes.
func (r *Reader) readLine() ([]byte, error) {
	var line []byte
	for {
		frag, err := r.br.ReadSlice('\n')
		if len(line)+len(frag) > MaxFrameBytes {
			r.dead = ErrReaderClosed
			return nil, fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, MaxFrameBytes)
		}
		line = append(line, frag...)
		switch {
		case err == nil:
			line = line[:len(line)-1] // drop the delimiter
			if n := len(line); n > 0 && line[n-1] == '\r' {
				line = line[:n-1]
			}
			if len(bytes.TrimSpace(line)) == 0 {
				line = line[:0]
				continue
			}
			return line, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			// A final line without a trailing newline is a truncated frame.
			if len(bytes.TrimSpace(line)) == 0 {
				return nil, io.EOF
			}
			return nil, io.ErrUnexpectedEOF
		default:
			return nil, err
		}
	}
}

// Writer encodes frames as NDJSON. It is safe for concurrent use.
type Writer struct {
	mu sync.Mutex
	w  io.Writer
}

// NewWriter returns a Writer over w.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// WriteFrame validates and writes f followed by a newline. An invalid frame is
// never partially written; the line reaches the connection as a single Write.
func (wr *Writer) WriteFrame(f Frame) error {
	if err := f.Validate(); err != nil {
		return err
	}
	buf, err := encodeLine(f)
	if err != nil {
		return err
	}
	if len(buf) > MaxFrameBytes {
		return fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, len(buf))
	}
	wr.mu.Lock()
	defer wr.mu.Unlock()
	_, err = wr.w.Write(buf)
	return err
}

// encodeLine renders f as one NDJSON line. Compaction is what makes
// newline-delimiting sound.
func encodeLine(f Frame) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	// HTML escaping would rewrite <, > and & inside a passed-through payload.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(f); err != nil {
		return nil, fmt.Errorf("librelay: encode frame %q: %w", f.Type, err)
	}
	return b.Bytes(), nil
}

// marshalCompact encodes v as compact, unescaped JSON with no trailing newline.
func marshalCompact(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}
