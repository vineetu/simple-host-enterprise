// Package scan is the optional malware scan of uploads: a minimal client for
// clamd's INSTREAM command over TCP. The server builds one only when
// CLAMD_ADDR is set; with none, uploads are not scanned.
package scan

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// Scanner reports whether a file is infected. Handlers take this interface
// so tests can supply a fake.
type Scanner interface {
	// Scan returns the signature clamd matched, or "" for a clean file. Any
	// error means the file could not be scanned; the caller refuses the
	// upload rather than storing something unscanned.
	Scan(ctx context.Context, r io.Reader) (string, error)
}

// DefaultChunkSize is how much of a file goes in one INSTREAM chunk.
const DefaultChunkSize = 64 << 10

// Clamd scans over one TCP connection per file.
type Clamd struct {
	// Addr is clamd's TCP host:port.
	Addr string
	// Timeout bounds one whole scan: connect, send and reply.
	Timeout time.Duration
	// ChunkSize is the INSTREAM chunk size; DefaultChunkSize when zero.
	ChunkSize int
}

// Scan streams r to clamd with zINSTREAM: each chunk is prefixed with its
// length as 4 big-endian bytes, and a zero length ends the stream. clamd
// answers "stream: OK" or "stream: <signature> FOUND", NUL-terminated.
func (c Clamd) Scan(ctx context.Context, r io.Reader) (string, error) {
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", c.Addr)
	if err != nil {
		return "", fmt.Errorf("clamd: connect: %w", err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	// Closing the connection is what unblocks a read or write when ctx is
	// cancelled without a deadline.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	if sendErr := c.send(conn, r); sendErr != nil {
		// clamd closes the stream when it refuses one (a file over its
		// StreamMaxLength, say) and says why; prefer that to the write error.
		if reply, readErr := readReply(conn); readErr == nil && reply != "" {
			return "", fmt.Errorf("clamd: %s", reply)
		}
		return "", fmt.Errorf("clamd: send: %w", sendErr)
	}
	reply, err := readReply(conn)
	if err != nil {
		return "", fmt.Errorf("clamd: read reply: %w", err)
	}
	return parseReply(reply)
}

func (c Clamd) send(conn net.Conn, r io.Reader) error {
	size := c.ChunkSize
	if size <= 0 {
		size = DefaultChunkSize
	}
	w := bufio.NewWriterSize(conn, size+4)
	if _, err := w.WriteString("zINSTREAM\x00"); err != nil {
		return err
	}
	chunk := make([]byte, size)
	var length [4]byte
	for {
		n, err := io.ReadFull(r, chunk)
		if n > 0 {
			binary.BigEndian.PutUint32(length[:], uint32(n))
			if _, werr := w.Write(length[:]); werr != nil {
				return werr
			}
			if _, werr := w.Write(chunk[:n]); werr != nil {
				return werr
			}
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	binary.BigEndian.PutUint32(length[:], 0)
	if _, err := w.Write(length[:]); err != nil {
		return err
	}
	return w.Flush()
}

// readReply reads up to the NUL that ends a z-command reply (or EOF), capped
// well above any real reply.
func readReply(conn net.Conn) (string, error) {
	data, err := bufio.NewReader(io.LimitReader(conn, 4096)).ReadString(0)
	if err != nil && (!errors.Is(err, io.EOF) || data == "") {
		return "", err
	}
	return strings.TrimSpace(strings.TrimSuffix(data, "\x00")), nil
}

func parseReply(reply string) (string, error) {
	body, ok := strings.CutPrefix(reply, "stream: ")
	switch {
	case !ok:
		return "", fmt.Errorf("clamd: unexpected reply %q", reply)
	case body == "OK":
		return "", nil
	case strings.HasSuffix(body, " FOUND"):
		return strings.TrimSuffix(body, " FOUND"), nil
	default:
		return "", fmt.Errorf("clamd: %s", body)
	}
}
