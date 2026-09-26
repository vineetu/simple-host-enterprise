package scan

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeClamd accepts connections, reads one zINSTREAM stream and answers with
// reply(content). It records the chunk lengths it saw.
type fakeClamd struct {
	listener net.Listener
	chunks   chan []int
	content  chan []byte
}

func startFakeClamd(t *testing.T, reply func([]byte) string) *fakeClamd {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeClamd{listener: listener, chunks: make(chan []int, 16), content: make(chan []byte, 16)}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go f.serve(conn, reply)
		}
	}()
	return f
}

func (f *fakeClamd) serve(conn net.Conn, reply func([]byte) string) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	command, err := r.ReadString(0)
	if err != nil || command != "zINSTREAM\x00" {
		_, _ = conn.Write([]byte("UNKNOWN COMMAND\x00"))
		return
	}
	var lengths []int
	var content bytes.Buffer
	for {
		var size [4]byte
		if _, err := io.ReadFull(r, size[:]); err != nil {
			return
		}
		n := int(binary.BigEndian.Uint32(size[:]))
		lengths = append(lengths, n)
		if n == 0 {
			break
		}
		if _, err := io.CopyN(&content, r, int64(n)); err != nil {
			return
		}
	}
	f.chunks <- lengths
	f.content <- content.Bytes()
	answer := reply(content.Bytes())
	if answer == "" {
		return // hang up without answering
	}
	_, _ = conn.Write([]byte(answer + "\x00"))
}

func TestClamdClean(t *testing.T) {
	fake := startFakeClamd(t, func([]byte) string { return "stream: OK" })
	sig, err := Clamd{Addr: fake.listener.Addr().String(), Timeout: 5 * time.Second}.Scan(context.Background(), strings.NewReader("hello"))
	if err != nil || sig != "" {
		t.Fatalf("Scan = %q, %v; want clean", sig, err)
	}
	if got := string(<-fake.content); got != "hello" {
		t.Fatalf("clamd received %q", got)
	}
}

func TestClamdFound(t *testing.T) {
	fake := startFakeClamd(t, func(content []byte) string {
		if bytes.Contains(content, []byte("EICAR")) {
			return "stream: Eicar-Test-Signature FOUND"
		}
		return "stream: OK"
	})
	sig, err := Clamd{Addr: fake.listener.Addr().String(), Timeout: 5 * time.Second}.Scan(context.Background(), strings.NewReader("x EICAR x"))
	if err != nil || sig != "Eicar-Test-Signature" {
		t.Fatalf("Scan = %q, %v; want the signature", sig, err)
	}
}

func TestClamdChunking(t *testing.T) {
	fake := startFakeClamd(t, func([]byte) string { return "stream: OK" })
	payload := bytes.Repeat([]byte("abcdefghij"), 25) // 250 bytes
	if _, err := (Clamd{Addr: fake.listener.Addr().String(), Timeout: 5 * time.Second, ChunkSize: 100}).Scan(context.Background(), bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	if got := <-fake.chunks; len(got) != 4 || got[0] != 100 || got[1] != 100 || got[2] != 50 || got[3] != 0 {
		t.Fatalf("chunk lengths = %v, want [100 100 50 0]", got)
	}
	if got := <-fake.content; !bytes.Equal(got, payload) {
		t.Fatalf("content mismatch")
	}

	// An empty file is just the terminator.
	if _, err := (Clamd{Addr: fake.listener.Addr().String(), Timeout: 5 * time.Second}).Scan(context.Background(), bytes.NewReader(nil)); err != nil {
		t.Fatal(err)
	}
	if got := <-fake.chunks; len(got) != 1 || got[0] != 0 {
		t.Fatalf("empty file chunks = %v", got)
	}
}

func TestClamdErrorReplies(t *testing.T) {
	for _, reply := range []string{"INSTREAM size limit exceeded. ERROR", "stream: Can't allocate memory ERROR", ""} {
		fake := startFakeClamd(t, func([]byte) string { return reply })
		sig, err := Clamd{Addr: fake.listener.Addr().String(), Timeout: 5 * time.Second}.Scan(context.Background(), strings.NewReader("x"))
		if err == nil {
			t.Fatalf("reply %q: Scan = %q, nil; want an error", reply, sig)
		}
	}
}

func TestClamdUnreachable(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	if _, err := (Clamd{Addr: addr, Timeout: 2 * time.Second}).Scan(context.Background(), strings.NewReader("x")); err == nil {
		t.Fatal("scan against a closed port succeeded")
	}
}

func TestClamdTimeout(t *testing.T) {
	// Accepts and reads, never answers.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(io.Discard, conn) }()
		}
	}()
	start := time.Now()
	_, err = Clamd{Addr: listener.Addr().String(), Timeout: 200 * time.Millisecond}.Scan(context.Background(), strings.NewReader("x"))
	if err == nil {
		t.Fatal("scan with no reply succeeded")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}
}
