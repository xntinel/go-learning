# Exercise 4: A Reproducible .golangci.yml on the v2 Schema

The point of a committed config is that every laptop and every CI runner reach the
same verdict. This exercise writes a v2 `.golangci.yml` — `version: "2"`,
`linters.default: none` plus an explicit enable list including `bodyclose` — around
a small HTTP client, validates it with `golangci-lint config verify`, and proves the
validation is real by feeding it a typo.

This module is self-contained: its own `go mod init`, an HTTP `fetch` package that
closes its response body, a demo driven by `httptest`, and a test.

## What you'll build

```text
fetchsvc/                     independent module: example.com/fetchsvc
  go.mod                      go 1.24
  fetch.go                    Get(ctx, client, url) []byte; closes resp.Body
  fetch_test.go               httptest server: 200 body + 500 -> ErrStatus
  cmd/
    demo/
      main.go                 spins up httptest, fetches, prints the body
  .golangci.yml               version "2", default none, explicit enable list
```

- Files: `fetch.go`, `fetch_test.go`, `cmd/demo/main.go`, plus the `.golangci.yml` shown in prose.
- Implement: `Get(ctx, client, url) ([]byte, error)` that builds a request, does it, always closes the body, and returns `ErrStatus` on a non-200.
- Test: an `httptest.Server` returning a fixed body (asserted exactly) and a second returning 500 (asserted via `errors.Is` against `ErrStatus`).
- Verify: `go test -count=1 -race ./...`; then `golangci-lint config verify` and `golangci-lint run ./...`.

### Why default: none plus an explicit enable list

v2 replaced the v1 `disable-all: true` / `enable-all: true` toggles with a single
`linters.default:` key taking one of four values: `none`, `standard`, `all`, or
`fast`. The most *predictable* choice for a team gate is `none` plus an explicit
`enable` list, because then the enabled set is exactly what you wrote — no
version-dependent default set can silently add or drop a linter when you upgrade the
tool. `standard` is a reasonable curated default, but it changes between releases;
`all` floods an existing codebase; `fast` is a speed-tuned subset. For a CI gate you
want the set to be an explicit, reviewed decision, and `none` + `enable` is how you
say that.

The enable list here is the high-value backend starter set: `errcheck` (unchecked
errors), `govet` (the `go vet` checks), `staticcheck` (which in v2 *absorbs* the old
`gosimple` and `stylecheck` — listing those separately is now an error), `unused`
(dead code), `ineffassign` (assignments never read), and `bodyclose` (unclosed HTTP
response bodies). `bodyclose` is the one that matters most for a service that makes
outbound HTTP calls: an unclosed `resp.Body` leaks a connection and a file
descriptor on every request, and under load the process runs out of descriptors and
starts failing every syscall. That is a real incident class, so it belongs in the
gate from day one.

### The HTTP client, written so bodyclose is satisfied

`Get` builds a context-aware request, does it, and defers closing the body. The
deferred close is what `bodyclose` looks for statically — remove it and the linter
reports "response body must be closed". Because the body is read fully before the
deferred close runs, the connection can be reused by the transport.

Create `fetch.go`:

```go
package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// ErrStatus is returned when the server responds with a non-200 status.
var ErrStatus = errors.New("unexpected status")

// Get fetches url with client and returns the response body. It always closes
// the response body (which the bodyclose linter verifies statically) and reads
// it fully so the underlying connection can be reused.
func Get(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %w", resp.Status, ErrStatus)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	return body, nil
}
```

### The config file

Create `.golangci.yml` at the module root:

```yaml
version: "2"

linters:
  default: none
  enable:
    - errcheck
    - govet
    - staticcheck
    - unused
    - ineffassign
    - bodyclose

run:
  timeout: 5m
```

Validate the file against the JSON schema before trusting it:

```bash
golangci-lint config verify
```

A valid file prints nothing and exits 0. Now prove the validation is real: change
`bodyclose` to a typo like `bodyclos` and re-run `config verify`. It rejects the
file with a schema error naming the unknown linter, instead of silently ignoring the
line — which is exactly the failure mode `config verify` exists to prevent. Restore
the correct name and the run is clean.

```bash
golangci-lint run ./...
```

With the fixed `fetch.go`, this reports nothing and exits 0.

### The runnable demo

The demo starts an in-process `httptest.Server`, fetches it, and prints the body, so
the output is deterministic and needs no network.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"

	"example.com/fetchsvc"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "pong")
	}))
	defer srv.Close()

	body, err := fetch.Get(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		return err
	}
	fmt.Printf("GET %s -> %s\n", "/", body)
	return nil
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
GET / -> pong
```

### Tests

The test uses two `httptest.Server`s: one that returns a fixed body (asserted
exactly) and one that returns 500 (asserted via `errors.Is` against `ErrStatus`).
`t.Context()` provides a context tied to the test lifetime.

Create `fetch_test.go`:

```go
package fetch

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGet(t *testing.T) {
	t.Parallel()

	t.Run("ok body", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("hello"))
		}))
		defer srv.Close()

		body, err := Get(t.Context(), srv.Client(), srv.URL)
		if err != nil {
			t.Fatalf("Get() err = %v", err)
		}
		if string(body) != "hello" {
			t.Fatalf("Get() body = %q, want %q", body, "hello")
		}
	})

	t.Run("non-200 -> ErrStatus", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		_, err := Get(t.Context(), srv.Client(), srv.URL)
		if !errors.Is(err, ErrStatus) {
			t.Fatalf("Get() err = %v, want ErrStatus", err)
		}
	})
}
```

## Review

The config is correct when `golangci-lint config verify` accepts it and
`golangci-lint run ./...` is clean on the module — and, critically, when a
deliberately misspelled linter name makes `config verify` *fail*, proving the file
is validated rather than silently ignored. `default: none` plus an explicit enable
list is what makes the run reproducible: the enabled set is exactly the six linters
listed, immune to default drift across tool versions. The `fetch` code is the reason
`bodyclose` earns a place in the set — its deferred `resp.Body.Close()` is what keeps
the linter quiet, and removing it is the exact leak that takes a service down under
load. The mistakes to avoid: listing `gosimple`/`stylecheck` alongside `staticcheck`
(a v2 error, since they merged), relying on `default: standard` and being surprised
when an upgrade changes the set, and forgetting `run.timeout` on a large repo where
the default (no timeout) can let a pathological run hang CI.

## Resources

- [golangci-lint: Configuration File (v2)](https://golangci-lint.run/docs/configuration/file/) — `version`, `linters.default`, `enable`, `run.timeout`.
- [golangci-lint: Command-Line (config verify)](https://golangci-lint.run/docs/configuration/cli/) — schema validation of the config.
- [bodyclose](https://github.com/timakin/bodyclose) — the analyzer that flags an unclosed `http.Response.Body`.
- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) — in-process servers for deterministic HTTP tests.

---

Back to [03-cli-error-surfacing.md](03-cli-error-surfacing.md) | Next: [05-expanding-linter-set-gocritic.md](05-expanding-linter-set-gocritic.md)
