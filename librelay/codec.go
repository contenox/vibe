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

// readBufSize is the initial bufio window. Frames are usually small — a
// heartbeat is under 64 bytes — and a long one grows the accumulator instead,
// so this is sized for the common case rather than for MaxFrameBytes.
const readBufSize = 64 * 1024

// Codec-level failures. ErrFrameTooLarge is the only one that ends a
// connection: every other error here is per-frame and the [Reader] resumes at
// the next line, because a peer that emits one bad frame has not proved the
// stream is unusable.
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

// ReadFrame returns the next frame.
//
// It distinguishes two kinds of failure, and callers must too. A malformed or
// invalid frame is reported with the line already consumed, so calling
// ReadFrame again is correct and reads the next frame; this is what lets one
// garbled message not take down a connection carrying other sessions.
// [ErrFrameTooLarge] is different: the offending line has not been consumed
// and cannot be, since consuming it is the unbounded read the limit exists to
// prevent. The Reader is dead after it and every later call returns
// [ErrReaderClosed] — resynchronizing on a newline an attacker chose would
// hand them control of where the next frame starts.
func (r *Reader) ReadFrame() (Frame, error) {
	if r.dead != nil {
		return Frame{}, r.dead
	}
	line, err := r.readLine()
	if err != nil {
		return Frame{}, err
	}
	var f Frame
	// Unknown fields are accepted deliberately: the envelope evolves by
	// addition, and DisallowUnknownFields would make every new field a
	// breaking change in the direction old→new.
	if err := json.Unmarshal(line, &f); err != nil {
		return Frame{}, fmt.Errorf("librelay: decode frame: %w", err)
	}
	if err := f.Validate(); err != nil {
		return Frame{}, err
	}
	return f, nil
}

// readLine returns one newline-delimited line with blank lines skipped,
// refusing to accumulate past MaxFrameBytes. The length check happens before
// the append, not after, so a hostile stream can never make this allocate more
// than one buffer growth beyond the limit.
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
			// ReadSlice reports nil only when it found the delimiter and
			// included it, so frag is non-empty and line is at least one byte
			// long. The subtraction cannot underflow — an empty line reaches
			// this branch as the single byte "\n", not as zero bytes.
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
			// A final line without a trailing newline is a
			// truncated frame, not a frame: the writer never omits
			// the delimiter, so its absence means the connection
			// died mid-write.
			if len(bytes.TrimSpace(line)) == 0 {
				return nil, io.EOF
			}
			return nil, io.ErrUnexpectedEOF
		default:
			return nil, err
		}
	}
}

// Writer encodes frames as NDJSON. It is safe for concurrent use: a connection
// is written by more than one goroutine (a session stream and a heartbeat, at
// minimum) and an interleaved frame is a corrupt stream, not a slow one.
type Writer struct {
	mu sync.Mutex
	w  io.Writer
}

// NewWriter returns a Writer over w.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// WriteFrame validates and writes f followed by a newline.
//
// It fails closed: an invalid frame is never partially written, because the
// encoding happens into a buffer and reaches the connection as a single Write.
// A frame that failed halfway across would desynchronize framing for every
// session sharing the connection, not just its own.
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
// newline-delimiting sound: a payload supplied as pretty-printed raw JSON
// would otherwise carry literal newlines and split one frame into several.
func encodeLine(f Frame) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	// HTML escaping would rewrite <, > and & inside an ACP payload the
	// relay promised to pass through untouched. Nothing here is embedded
	// in a document, so the escaping buys nothing and costs byte fidelity.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(f); err != nil {
		return nil, fmt.Errorf("librelay: encode frame %q: %w", f.Type, err)
	}
	return b.Bytes(), nil
}

// marshalCompact encodes v as compact, unescaped JSON with no trailing
// newline — the form a payload must be in before it is embedded in a frame.
func marshalCompact(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}
