# 11. HTTP/2 Server Push

Build a small library package that serves an HTML page and attempts to push its CSS and JavaScript dependencies. The package must work when push is unavailable, report push failures as wrapped sentinel errors, and remain testable with only `net/http` and `net/http/httptest`.

## Concepts

### Push Is An Optional ResponseWriter Capability

`net/http` exposes HTTP/2 server push through the `http.Pusher` interface. A handler must check `pusher, ok := w.(http.Pusher)` because ordinary `httptest.ResponseRecorder` values and HTTP/1.x response writers do not support push.

### Push Before Writing The Body

Push attempts belong before the HTML response body is written. Once a handler starts writing, headers may be committed and a real server has less freedom to attach associated streams.

### Test The Contract, Not Browser Behavior

Most browsers no longer benefit from HTTP/2 server push, and `httptest` does not simulate browser push caches. A reliable unit test uses a custom response writer that implements `http.Pusher` and records the requested targets.

## Exercises

Create `go.mod`:

```go
module http2serverpush

go 1.26
```

Create `pushserver.go`:

```go
package http2serverpush

import (
	"errors"
	"fmt"
	"io"
	"net/http"
)

var ErrPushFailed = errors.New("push failed")

var pushTargets = []string{
	"/static/style.css",
	"/static/app.js",
}

const htmlPage = `<!doctype html>
<html lang="en">
<head>
	<meta charset="utf-8">
	<title>HTTP/2 Server Push</title>
	<link rel="stylesheet" href="/static/style.css">
</head>
<body>
	<h1>HTTP/2 Server Push</h1>
	<p id="message">Waiting for JavaScript</p>
	<script src="/static/app.js"></script>
</body>
</html>
`

const styleCSS = `body {
	font-family: sans-serif;
	margin: 2rem;
}

h1 {
	color: #1f2937;
}
`

const appJS = `document.getElementById("message").textContent = "Resources are available";
`

type Server struct{}

func New() Server {
	return Server{}
}

func (s Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.index)
	mux.HandleFunc("GET /static/style.css", serveString("text/css; charset=utf-8", styleCSS))
	mux.HandleFunc("GET /static/app.js", serveString("text/javascript; charset=utf-8", appJS))
	return mux
}

func (s Server) index(w http.ResponseWriter, r *http.Request) {
	if err := PushResources(w); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, htmlPage)
}

func serveString(contentType string, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		_, _ = io.WriteString(w, body)
	}
}

func PushResources(w http.ResponseWriter) error {
	pusher, ok := w.(http.Pusher)
	if !ok {
		return nil
	}

	for _, target := range pushTargets {
		if err := pusher.Push(target, nil); err != nil {
			return fmt.Errorf("%w: %s: %w", ErrPushFailed, target, err)
		}
	}

	return nil
}
```

Create `pushserver_test.go`:

```go
package http2serverpush

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

var errInjectedPush = errors.New("injected push failure")

func TestHandlerRoutes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		path        string
		wantStatus  int
		contentType string
		body        string
	}{
		{
			name:        "index",
			path:        "/",
			wantStatus:  http.StatusOK,
			contentType: "text/html; charset=utf-8",
			body:        "HTTP/2 Server Push",
		},
		{
			name:        "css",
			path:        "/static/style.css",
			wantStatus:  http.StatusOK,
			contentType: "text/css; charset=utf-8",
			body:        "font-family",
		},
		{
			name:        "js",
			path:        "/static/app.js",
			wantStatus:  http.StatusOK,
			contentType: "text/javascript; charset=utf-8",
			body:        "Resources are available",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			rec := httptest.NewRecorder()

			New().Handler().ServeHTTP(rec, req)

			res := rec.Result()
			defer res.Body.Close()

			body, err := io.ReadAll(res.Body)
			if err != nil {
				t.Fatalf("read response body: %v", err)
			}

			if res.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", res.StatusCode, tt.wantStatus)
			}

			if got := res.Header.Get("Content-Type"); got != tt.contentType {
				t.Fatalf("content type = %q, want %q", got, tt.contentType)
			}

			if !strings.Contains(string(body), tt.body) {
				t.Fatalf("body %q does not contain %q", string(body), tt.body)
			}
		})
	}
}

func TestPushResources(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		writer     http.ResponseWriter
		wantPushes []string
		wantErr    error
	}{
		{
			name:       "no pusher is skipped",
			writer:     httptest.NewRecorder(),
			wantPushes: nil,
			wantErr:    nil,
		},
		{
			name:       "pusher records resources",
			writer:     &pushRecorder{ResponseRecorder: httptest.NewRecorder()},
			wantPushes: []string{"/static/style.css", "/static/app.js"},
			wantErr:    nil,
		},
		{
			name:       "push failure is wrapped",
			writer:     &pushRecorder{ResponseRecorder: httptest.NewRecorder(), err: errInjectedPush},
			wantPushes: []string{"/static/style.css"},
			wantErr:    ErrPushFailed,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := PushResources(tt.writer)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want errors.Is(..., %v)", err, tt.wantErr)
			}

			recorder, ok := tt.writer.(*pushRecorder)
			if !ok {
				return
			}

			if !reflect.DeepEqual(recorder.pushes, tt.wantPushes) {
				t.Fatalf("pushes = %#v, want %#v", recorder.pushes, tt.wantPushes)
			}
		})
	}
}

func TestHandlerReportsPushFailure(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := &pushRecorder{ResponseRecorder: httptest.NewRecorder(), err: errInjectedPush}

	New().Handler().ServeHTTP(rec, req)

	res := rec.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusInternalServerError)
	}
}

type pushRecorder struct {
	*httptest.ResponseRecorder
	pushes []string
	err    error
}

func (r *pushRecorder) Push(target string, opts *http.PushOptions) error {
	r.pushes = append(r.pushes, target)
	if r.err != nil {
		return r.err
	}
	return nil
}
```

Create `example_test.go`:

```go
package http2serverpush

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
)

func ExampleServer_Handler() {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	New().Handler().ServeHTTP(rec, req)

	res := rec.Result()
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)

	fmt.Println(res.StatusCode)
	fmt.Println(res.Header.Get("Content-Type"))
	fmt.Println(len(body) > 0)

	// Output:
	// 200
	// text/html; charset=utf-8
	// true
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"log"
	"net/http"

	"http2serverpush"
)

func main() {
	server := &http.Server{
		Addr:    ":8443",
		Handler: http2serverpush.New().Handler(),
	}

	log.Println("serving https://localhost:8443")
	if err := server.ListenAndServeTLS("server.crt", "server.key"); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
```

For the demo command, generate a local certificate before running the server:

```bash
openssl req -x509 -newkey rsa:2048 -nodes -keyout server.key -out server.crt -days 365 -subj "/CN=localhost"
go run ./cmd/demo
```

## Common Mistakes

- Wrong: Calling `Push` without checking whether the response writer implements `http.Pusher`.
- What happens: The handler panics or couples itself to HTTP/2-only behavior that ordinary `httptest` recorders cannot provide.
- Fix: Use `pusher, ok := w.(http.Pusher)` and make no-push behavior a valid path.
- Wrong: Testing push by requiring a browser or command-line HTTP/2 client.
- What happens: Unit tests become environment-dependent and fail for reasons unrelated to handler behavior.
- Fix: Use `httptest` for normal responses and a custom response writer that records `Push` calls.
- Wrong: Returning raw push errors.
- What happens: The caller cannot distinguish a push failure from unrelated I/O failures without string matching.
- Fix: Return `fmt.Errorf("push %s: %w", target, ErrPushFailed)` and assert with `errors.Is`.

## Verification

Run these commands from the module root:

```bash
gofmt -l .
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Expected results:

- `gofmt -l .` prints nothing.
- `go vet ./...` exits successfully.
- `go build ./...` exits successfully.
- `go test -count=1 -race ./...` exits successfully.

## Summary

- `http.Pusher` is optional; handlers must detect it with a type assertion.
- Push attempts should occur before the HTML response body is written.
- A custom response writer is the reliable way to unit-test push behavior without depending on browser support.

## What's Next

Next: [Reverse Proxy and Load Balancer](../12-reverse-proxy-and-load-balancer/12-reverse-proxy-and-load-balancer.md).

## Resources

- [`net/http`](https://pkg.go.dev/net/http)
- [`http.Pusher`](https://pkg.go.dev/net/http#Pusher)
- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest)
- [`httptest.ResponseRecorder`](https://pkg.go.dev/net/http/httptest#ResponseRecorder)
- [`httptest.NewUnstartedServer`](https://pkg.go.dev/net/http/httptest#NewUnstartedServer)
