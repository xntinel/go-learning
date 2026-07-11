# Exercise 7: Attach Delve to a Live HTTP Server

The production-flavored debugging move is not launching a fresh process — it is
stopping one that is already serving traffic. `dlv attach <pid>` freezes a running
server so you can set a breakpoint in a handler, send a request that stops it
mid-flight, inspect the in-flight `*http.Request`, and then detach without killing
the process. This module builds that server and walks the attach workflow.

This module is fully self-contained: its own `go mod init`, demo, and test.

## What you'll build

```text
livesrv/                   independent module: example.com/livesrv
  go.mod                   go 1.24
  server/
    server.go              New() http.Handler; handleOrder with PathValue routing
  cmd/
    demo/
      main.go              self-test mode (default) and `serve` mode for attach
  server/server_test.go    httptest-driven table test + Example
```

- Files: `server/server.go`, `cmd/demo/main.go`, `server/server_test.go`.
- Implement: `New() http.Handler` with a `GET /orders/{id}` route returning JSON and a `GET /healthz` route.
- Test: `httptest.NewServer` exercising both routes and a 404; table-driven.
- Verify: `go test -count=1 -race ./...`, then a scripted `dlv attach` that breaks in the handler and prints the request path while the server keeps running.

Set up the module:

```bash
mkdir -p ~/go-exercises/livesrv/server ~/go-exercises/livesrv/cmd/demo
cd ~/go-exercises/livesrv
go mod init example.com/livesrv
go mod edit -go=1.24
```

### Why attach, and the source-to-binary contract

`dlv debug` restarts the program; a production server cannot be restarted just to
inspect one request. `dlv attach <pid>` stops the live process in place using the
OS ptrace facility, so you debug the exact process serving traffic. Two conditions
must hold. First, the binary must be built with `-gcflags='all=-N -l'`, because
attach consumes the running binary as-is — an optimized build shows `<optimized
out>` for the request fields you want to read. Second, the source you inspect must
match the binary's commit, or the DWARF line tables point at the wrong lines. When
you finish, you must detach rather than kill: quitting the session with a detach
leaves the process running, while a kill takes your server down.

Create `server/server.go`:

```go
package server

import (
	"encoding/json"
	"net/http"
)

type orderResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// New returns the service's HTTP handler: an order lookup and a health check.
func New() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /orders/{id}", handleOrder)
	mux.HandleFunc("GET /healthz", handleHealth)
	return mux
}

// handleOrder echoes the path id back as JSON. It is the function you break on
// when attached: r carries the in-flight request.
func handleOrder(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(orderResponse{ID: id, Status: "ok"})
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}
```

### The attach workflow

Build with debug info and start the server in the background in `serve` mode, then
attach to its PID:

```bash
go build -gcflags='all=-N -l' -o /tmp/livesrv ./cmd/demo
/tmp/livesrv serve &        # long-running server on :8080
SRV=$!
curl -s localhost:8080/healthz   # confirm it is alive -> ok

dlv attach "$SRV"
```

Inside the session, break in the handler and continue; the server is now waiting
for a request. In another terminal, `curl -s localhost:8080/orders/42` — the
handler stops mid-flight and you inspect the request:

```text
(dlv) break example.com/livesrv/server.handleOrder
Breakpoint 1 set at 0x... for example.com/livesrv/server.handleOrder() ./server/server.go:24
(dlv) continue
> example.com/livesrv/server.handleOrder() ./server/server.go:24 (hits goroutine(34):1 total:1)
(dlv) print r.URL.Path
"/orders/42"
(dlv) print r.Method
"GET"
(dlv) print r.Header["User-Agent"]
[]string len: 1, cap: 1, ["curl/8.7.1"]
(dlv) continue
(dlv) quit
Would you like to kill the process? [Y/n] n
```

`print r.URL.Path` reads the in-flight request's path — `/orders/42` — from the
live process; `r.Header` decodes as a real `http.Header` (a `map[string][]string`)
because Delve understands the type. Answering `n` to the kill prompt detaches and
leaves the server running, which you confirm with a second curl that still returns
200. That last step is the operational discipline: never take down a live process
by quitting carelessly.

### The demo: self-test mode

Create `cmd/demo/main.go`. With no arguments it runs an in-process self-test and
prints a deterministic result; with `serve` it runs the long-lived server used by
the attach workflow above.

```go
package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"

	"example.com/livesrv/server"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		srv := &http.Server{Addr: ":8080", Handler: server.New()}
		log.Fatal(srv.ListenAndServe())
		return
	}

	// Default: self-test against an in-process listener on an ephemeral port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	srv := &http.Server{Handler: server.New()}
	go srv.Serve(ln)
	defer srv.Close()

	resp, err := http.Get("http://" + ln.Addr().String() + "/orders/7")
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	fmt.Printf("GET /orders/7 -> %d\n", resp.StatusCode)
	fmt.Print(string(body))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
GET /orders/7 -> 200
{"id":"7","status":"ok"}
```

### The test drives the handler with httptest

Create `server/server_test.go`:

```go
package server

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRoutes(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(New())
	t.Cleanup(ts.Close)

	tests := []struct {
		name       string
		path       string
		wantStatus int
		wantBody   string
	}{
		{name: "order", path: "/orders/42", wantStatus: 200, wantBody: `{"id":"42","status":"ok"}`},
		{name: "health", path: "/healthz", wantStatus: 200, wantBody: "ok"},
		{name: "missing", path: "/nope", wantStatus: 404},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resp, err := http.Get(ts.URL + tc.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.path, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("GET %s status = %d; want %d", tc.path, resp.StatusCode, tc.wantStatus)
			}
			if tc.wantBody != "" {
				body, _ := io.ReadAll(resp.Body)
				if got := strings.TrimSpace(string(body)); got != tc.wantBody {
					t.Fatalf("GET %s body = %q; want %q", tc.path, got, tc.wantBody)
				}
			}
		})
	}
}

func ExampleNew() {
	ts := httptest.NewServer(New())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/orders/7")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	fmt.Print(string(body))
	// Output: {"id":"7","status":"ok"}
}
```

### Scripted: attach, print the path, survive detach

```bash
go build -gcflags='all=-N -l' -o /tmp/livesrv ./cmd/demo
/tmp/livesrv serve & SRV=$!
sleep 1
curl -s localhost:8080/healthz >/dev/null   # liveness

cat > /tmp/srv.dlv <<'EOF'
break example.com/livesrv/server.handleOrder
continue
print r.URL.Path
detach
EOF

( sleep 1; curl -s localhost:8080/orders/42 >/dev/null ) &   # trigger the handler
dlv attach "$SRV" --init /tmp/srv.dlv 2>&1 | tee /tmp/srv.out

grep -q '/orders/42' /tmp/srv.out && echo "path captured"
curl -s -o /dev/null -w '%{http_code}\n' localhost:8080/healthz   # still 200 after detach
kill "$SRV"
```

The scripted session breaks in `handleOrder`, prints the in-flight path, and
`detach` leaves the process running — the final curl returns `200`, proving the
server survived the debugging session.

## Review

The server is correct when `GET /orders/{id}` echoes the path id as JSON and
unknown paths 404, which `httptest.NewServer` and the table pin including the 404
case. The attach proof is reading `r.URL.Path` from the live process while a real
curl is blocked in the handler — that only works if the binary was built with
`-N -l` and the source matches. The operational mistakes to avoid are attaching to
an optimized binary (request fields read `<optimized out>`) and quitting the
session in a way that kills the server; answer `n` to the kill prompt, or script
`detach`, so the process keeps serving. `PathValue` is the Go 1.22+ router's way to
read `{id}` — no third-party router needed.

## Resources

- [`dlv attach` usage](https://github.com/go-delve/delve/blob/master/Documentation/usage/dlv_attach.md) — attaching to a running process and the detach-vs-kill choice.
- [`net/http.ServeMux`](https://pkg.go.dev/net/http#ServeMux) — the 1.22+ method/pattern routing and `Request.PathValue`.
- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) — `NewServer` for driving the real handler in tests.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-remote-headless-connect.md](08-remote-headless-connect.md)
