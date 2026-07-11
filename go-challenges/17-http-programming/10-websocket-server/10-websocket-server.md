# 10. WebSocket Server

Implement a small, testable WebSocket teaching package with only the Go standard library. The hard part is not the happy-path echo; it is knowing where `net/http` stops, when an HTTP connection can be hijacked, and which frame and handshake errors must be observable in tests.

## Concepts

The Go standard library includes HTTP servers, clients, `httptest`, and connection hijacking, but it does not include a complete WebSocket API. Production systems normally use a maintained WebSocket package. This exercise is a teaching artifact that implements the minimum RFC 6455 pieces needed for a single-message echo server: HTTP upgrade validation, `Sec-WebSocket-Accept`, masked client text frames, unmasked server text frames, and close handling.

`http.Hijacker` is implemented by HTTP/1.x server response writers and returns the underlying `net.Conn` plus buffered reader/writer. After hijacking, the HTTP server no longer owns the connection. RFC 6455 requires the server handshake to return status `101 Switching Protocols`, `Upgrade: websocket`, `Connection: Upgrade`, and `Sec-WebSocket-Accept`, where the accept value is `base64(sha1(Sec-WebSocket-Key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))`.

## Exercises

Create this module layout:

```text
websocket-server/
  go.mod
  ws.go
  example_test.go
  ws_test.go
  cmd/demo/main.go
```

Create `go.mod`:

```go.mod
module websocket-server

go 1.26
```

Create `ws.go`:

```go
package websocketserver

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const websocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

const (
	opcodeClose = 0x8
	opcodeText  = 0x1
)

var (
	ErrBadHandshake      = errors.New("bad websocket handshake")
	ErrHijackUnsupported = errors.New("websocket hijack unsupported")
	ErrUnsupportedFrame  = errors.New("unsupported websocket frame")
)

type Frame struct {
	Opcode  byte
	Payload []byte
}

func AcceptKey(key string) string {
	h := sha1.Sum([]byte(strings.TrimSpace(key) + websocketGUID))
	return base64.StdEncoding.EncodeToString(h[:])
}

func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := validateHandshake(r); err != nil {
			http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
			return
		}

		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			if errors.Is(err, http.ErrNotSupported) {
				http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
				return
			}
			return
		}
		defer conn.Close()

		accept := AcceptKey(r.Header.Get("Sec-WebSocket-Key"))
		_, err = fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept)
		if err != nil {
			return
		}
		if err := rw.Flush(); err != nil {
			return
		}

		for {
			frame, err := ReadFrame(rw.Reader)
			if err != nil {
				return
			}
			if frame.Opcode == opcodeClose {
				_ = WriteTextFrame(rw.Writer, []byte{})
				_ = rw.Flush()
				return
			}
			if frame.Opcode != opcodeText {
				return
			}
			if err := WriteTextFrame(rw.Writer, frame.Payload); err != nil {
				return
			}
			if err := rw.Flush(); err != nil {
				return
			}
		}
	})
}

func validateHandshake(r *http.Request) error {
	if r.Method != http.MethodGet {
		return fmt.Errorf("%w: method must be GET", ErrBadHandshake)
	}
	if !containsToken(r.Header.Get("Connection"), "upgrade") {
		return fmt.Errorf("%w: missing Connection upgrade token", ErrBadHandshake)
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return fmt.Errorf("%w: missing Upgrade websocket", ErrBadHandshake)
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		return fmt.Errorf("%w: unsupported websocket version", ErrBadHandshake)
	}
	if r.Header.Get("Sec-WebSocket-Key") == "" {
		return fmt.Errorf("%w: missing websocket key", ErrBadHandshake)
	}
	return nil
}

func containsToken(value, want string) bool {
	for _, token := range strings.Split(value, ",") {
		if strings.EqualFold(strings.TrimSpace(token), want) {
			return true
		}
	}
	return false
}

func ReadFrame(r *bufio.Reader) (Frame, error) {
	header, err := r.Peek(2)
	if err != nil {
		return Frame{}, fmt.Errorf("read frame header: %w", err)
	}
	_, _ = r.Discard(2)

	opcode := header[0] & 0x0f
	masked := header[1]&0x80 != 0
	length := uint64(header[1] & 0x7f)
	if !masked {
		return Frame{}, fmt.Errorf("%w: client frames must be masked", ErrUnsupportedFrame)
	}
	if length == 126 {
		var b [2]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return Frame{}, fmt.Errorf("read extended length: %w", err)
		}
		length = uint64(binary.BigEndian.Uint16(b[:]))
	}
	if length == 127 {
		return Frame{}, fmt.Errorf("%w: 64-bit lengths are not implemented", ErrUnsupportedFrame)
	}

	var mask [4]byte
	if _, err := io.ReadFull(r, mask[:]); err != nil {
		return Frame{}, fmt.Errorf("read mask: %w", err)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return Frame{}, fmt.Errorf("read payload: %w", err)
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return Frame{Opcode: opcode, Payload: payload}, nil
}

func WriteTextFrame(w *bufio.Writer, payload []byte) error {
	if len(payload) > 125 {
		return fmt.Errorf("%w: payload too large for teaching frame", ErrUnsupportedFrame)
	}
	if err := w.WriteByte(0x80 | opcodeText); err != nil {
		return fmt.Errorf("write opcode: %w", err)
	}
	if err := w.WriteByte(byte(len(payload))); err != nil {
		return fmt.Errorf("write length: %w", err)
	}
	if _, err := w.Write(payload); err != nil {
		return fmt.Errorf("write payload: %w", err)
	}
	return nil
}

func MaskedTextFrame(payload []byte) []byte {
	mask := [4]byte{1, 2, 3, 4}
	frame := []byte{0x80 | opcodeText, 0x80 | byte(len(payload))}
	frame = append(frame, mask[:]...)
	for i, b := range payload {
		frame = append(frame, b^mask[i%4])
	}
	return frame
}
```

Create `example_test.go`:

```go
package websocketserver_test

import (
	"fmt"

	websocketserver "websocket-server"
)

func ExampleAcceptKey() {
	fmt.Println(websocketserver.AcceptKey("dGhlIHNhbXBsZSBub25jZQ=="))
	// Output: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=
}
```

Create `ws_test.go`:

```go
package websocketserver

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAcceptKey(t *testing.T) {
	t.Parallel()

	got := AcceptKey("dGhlIHNhbXBsZSBub25jZQ==")
	want := "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="
	if got != want {
		t.Fatalf("AcceptKey() = %q, want %q", got, want)
	}
}

func TestValidateHandshake(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*http.Request)
		wantErr bool
	}{
		{
			name: "valid",
		},
		{
			name: "wrong method",
			mutate: func(r *http.Request) {
				r.Method = http.MethodPost
			},
			wantErr: true,
		},
		{
			name: "missing key",
			mutate: func(r *http.Request) {
				r.Header.Del("Sec-WebSocket-Key")
			},
			wantErr: true,
		},
		{
			name: "wrong version",
			mutate: func(r *http.Request) {
				r.Header.Set("Sec-WebSocket-Version", "12")
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := newHandshakeRequest()
			if tt.mutate != nil {
				tt.mutate(req)
			}
			err := validateHandshake(req)
			if tt.wantErr {
				if !errors.Is(err, ErrBadHandshake) {
					t.Fatalf("validateHandshake() error = %v, want ErrBadHandshake", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateHandshake() error = %v", err)
			}
		})
	}
}

func TestReadFrame(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		frame   []byte
		want    string
		wantErr error
	}{
		{
			name:  "masked text",
			frame: MaskedTextFrame([]byte("hello")),
			want:  "hello",
		},
		{
			name:    "unmasked client frame",
			frame:   []byte{0x81, 0x02, 'n', 'o'},
			wantErr: ErrUnsupportedFrame,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			frame, err := ReadFrame(bufio.NewReader(strings.NewReader(string(tt.frame))))
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("ReadFrame() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadFrame() error = %v", err)
			}
			if string(frame.Payload) != tt.want {
				t.Fatalf("ReadFrame() payload = %q, want %q", frame.Payload, tt.want)
			}
		})
	}
}

func TestWriteTextFrame(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	w := bufio.NewWriter(&b)
	if err := WriteTextFrame(w, []byte("ok")); err != nil {
		t.Fatalf("WriteTextFrame() error = %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	want := string([]byte{0x81, 0x02, 'o', 'k'})
	if b.String() != want {
		t.Fatalf("frame bytes = %v, want %v", []byte(b.String()), []byte(want))
	}
}

func TestHandlerRejectsBadHandshakeWithHTTPRecorder(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	rr := httptest.NewRecorder()

	Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestHandlerHandshakeAndEchoWithHTTPServer(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(Handler())
	t.Cleanup(server.Close)

	addr := strings.TrimPrefix(server.URL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetDeadline() error = %v", err)
	}

	key := "dGhlIHNhbXBsZSBub25jZQ=="
	request := "GET /ws HTTP/1.1\r\n" +
		"Host: " + addr + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: keep-alive, Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatalf("WriteString() error = %v", err)
	}

	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("ReadResponse() error = %v", err)
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusSwitchingProtocols)
	}
	if got := response.Header.Get("Sec-WebSocket-Accept"); got != AcceptKey(key) {
		t.Fatalf("accept = %q, want %q", got, AcceptKey(key))
	}

	if _, err := conn.Write(MaskedTextFrame([]byte("ping"))); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	echoHeader := make([]byte, 2)
	if _, err := io.ReadFull(reader, echoHeader); err != nil {
		t.Fatalf("ReadFull(header) error = %v", err)
	}
	if echoHeader[0] != 0x81 || echoHeader[1] != 0x04 {
		t.Fatalf("echo header = %v, want [129 4]", echoHeader)
	}
	payload := make([]byte, 4)
	if _, err := io.ReadFull(reader, payload); err != nil {
		t.Fatalf("ReadFull(payload) error = %v", err)
	}
	if string(payload) != "ping" {
		t.Fatalf("payload = %q, want ping", payload)
	}
}

func TestNetPipeFrameRoundTrip(t *testing.T) {
	t.Parallel()

	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close() })
	t.Cleanup(func() { _ = client.Close() })

	errc := make(chan error, 1)
	go func() {
		r := bufio.NewReader(server)
		w := bufio.NewWriter(server)
		frame, err := ReadFrame(r)
		if err != nil {
			errc <- err
			return
		}
		if err := WriteTextFrame(w, frame.Payload); err != nil {
			errc <- err
			return
		}
		errc <- w.Flush()
	}()

	if _, err := client.Write(MaskedTextFrame([]byte("pipe"))); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	reader := bufio.NewReader(client)
	header := make([]byte, 2)
	if _, err := io.ReadFull(reader, header); err != nil {
		t.Fatalf("ReadFull(header) error = %v", err)
	}
	payload := make([]byte, int(header[1]))
	if _, err := io.ReadFull(reader, payload); err != nil {
		t.Fatalf("ReadFull(payload) error = %v", err)
	}
	if string(payload) != "pipe" {
		t.Fatalf("payload = %q, want pipe", payload)
	}
	if err := <-errc; err != nil {
		t.Fatalf("server error = %v", err)
	}
}

func newHandshakeRequest() *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	req.Header.Set("Connection", "keep-alive, Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	req.Header.Set("Sec-WebSocket-Version", "13")
	return req
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"log"
	"net/http"

	websocketserver "websocket-server"
)

func main() {
	mux := http.NewServeMux()
	mux.Handle("/ws", websocketserver.Handler())

	log.Println("listening on http://localhost:8080/ws")
	log.Fatal(http.ListenAndServe(":8080", mux))
}
```

## Common Mistakes

- Wrong: Treating WebSocket as a normal HTTP response after the upgrade.
- What happens: The server writes bytes through `ResponseWriter` after ownership should have moved to the raw connection, making frame behavior impossible to reason about.
- Fix: Validate the upgrade first, call `http.NewResponseController(w).Hijack()`, flush the `101 Switching Protocols` response, and then use the hijacked reader and writer only.
- Wrong: Accepting unmasked client frames.
- What happens: The implementation accepts frames that real WebSocket clients are required to mask, so protocol violations pass tests.
- Fix: Return an error wrapping `ErrUnsupportedFrame` when the client mask bit is absent.
- Wrong: Matching error strings in tests.
- What happens: A useful contextual error message breaks tests even though the underlying failure is unchanged.
- Fix: Wrap sentinel errors with `%w` and assert with `errors.Is`.

## Verification

Run these checks from the module root:

```bash
gofmt -l .
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Expected results:

- `gofmt -l .` prints nothing
- `go vet ./...` exits successfully
- `go build ./...` exits successfully
- `go test -count=1 -race ./...` exits successfully

## Summary

- The standard library can host the HTTP upgrade boundary, but it does not provide a full WebSocket implementation.
- `http.NewResponseController(w).Hijack()` transfers ownership of an HTTP/1.x connection to the handler.
- Tests should prove handshake validation, frame masking rules, and wrapped sentinel errors.

## What's Next

Next: [HTTP/2 Server Push](../11-http2-server-push/11-http2-server-push.md).

## Resources

- [net/http Hijacker](https://pkg.go.dev/net/http#Hijacker)
- [net/http ResponseController.Hijack](https://pkg.go.dev/net/http#ResponseController.Hijack)
- [RFC 6455: The WebSocket Protocol](https://www.rfc-editor.org/rfc/rfc6455)
