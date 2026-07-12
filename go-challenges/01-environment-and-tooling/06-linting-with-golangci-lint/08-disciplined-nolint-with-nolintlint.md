# Exercise 8: Disciplined Suppression Enforced by nolintlint

Suppression is an exception, not an escape hatch. This exercise builds a health-check
client whose deferred body close is a genuine best-effort operation, converts that
into an *auditable* `//nolint:errcheck // reason` directive, and configures
`nolintlint` so bare or reasonless suppressions are themselves flagged.

This module is self-contained: its own `go mod init`, an `httpcheck` package, a demo
driven by `httptest`, and a test.

## What you'll build

```text
nolintdemo/                   independent module: example.com/nolintdemo
  go.mod                      go 1.24
  httpcheck.go                Ping(ctx, client, url) bool; audited nolint close
  httpcheck_test.go           httptest: 2xx -> true, 5xx -> false
  cmd/
    demo/
      main.go                 pings an httptest server
  .golangci.yml               nolintlint settings (shown in prose)
```

- Files: `httpcheck.go`, `httpcheck_test.go`, `cmd/demo/main.go`, plus the config in prose.
- Implement: `Ping(ctx, client, url) (bool, error)` that reports whether the endpoint answered 2xx, draining and closing the body on a best-effort basis behind an audited `//nolint`.
- Test: an `httptest.Server` returning 200 (true) and one returning 500 (false).
- Verify: `go test -count=1 -race ./...`; then `golangci-lint run ./...` with `nolintlint` on.

### When a suppression is legitimate — and how to make it auditable

`Ping` only cares about the *status* of an endpoint, not its body. It still must
drain and close the body so the underlying connection can be reused, but the close
error is genuinely not actionable: the body was already consumed, and there is
nothing sensible to do if the close fails. This is the rare case where ignoring an
error is correct — and `errcheck` will still flag the bare `resp.Body.Close()`.

The wrong reactions are a bare `//nolint` (silences *every* linter on the line, hides
future bugs) and a `//nolint:errcheck` with no explanation (specific, but the next
reader cannot tell whether it is a considered decision or a shortcut). The right
directive names the exact linter and states the reason, so a reviewer can audit it:

```go
	//nolint:errcheck // best-effort: body already drained, close error is not actionable
	resp.Body.Close()
```

`nolintlint` is the linter that *enforces* this shape. With `require-specific: true`
a bare `//nolint` is rejected; with `require-explanation: true` a directive without a
trailing comment is rejected; and with `allow-unused: false` a stale directive that
no longer suppresses anything is flagged so it gets deleted. The suppression stops
being a quiet override and becomes a reviewable, self-cleaning exception.

Create `httpcheck.go`:

```go
package httpcheck

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// Ping reports whether url answered with a 2xx status. It does not use the body,
// but drains and closes it so the connection can be reused; the close is a
// deliberate best-effort operation, suppressed with an audited nolint directive.
func Ping(ctx context.Context, client *http.Client, url string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, fmt.Errorf("build request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("do request: %w", err)
	}
	// Drain so keep-alive can reuse the connection, then close best-effort.
	_, _ = io.Copy(io.Discard, resp.Body)
	//nolint:errcheck // best-effort: body already drained, close error is not actionable
	resp.Body.Close()

	return resp.StatusCode >= 200 && resp.StatusCode < 300, nil
}
```

### The config

Create `.golangci.yml` enabling `nolintlint` with a strict policy:

```yaml
version: "2"

linters:
  default: none
  enable:
    - errcheck
    - govet
    - bodyclose
    - nolintlint
  settings:
    nolintlint:
      require-explanation: true
      require-specific: true
      allow-unused: false
```

Now exercise the policy. The audited directive above passes. Replace it with a bare
`//nolint` and `nolintlint` reports "directive `//nolint` should mention specific
linter such as `//nolint:errcheck`". Replace it with `//nolint:errcheck` and no
comment and `nolintlint` reports "directive `//nolint:errcheck` should provide
explanation". Move the directive to a line that has no `errcheck` finding and, with
`allow-unused: false`, `nolintlint` reports the directive as unused so you delete it.
Each variant is caught, which is the point: the suppression policy is enforced by a
linter, not by reviewer memory.

```bash
golangci-lint run ./...
```

With the audited directive, the run is clean.

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"

	"example.com/nolintdemo"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	ok, err := httpcheck.Ping(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		return err
	}
	fmt.Printf("healthy: %t\n", ok)
	return nil
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
healthy: true
```

### Tests

Create `httpcheck_test.go`. Two servers exercise the 2xx and 5xx branches; the body
is intentionally non-empty so the drain path actually runs.

```go
package httpcheck

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
		want   bool
	}{
		{name: "ok", status: http.StatusOK, want: true},
		{name: "no content", status: http.StatusNoContent, want: true},
		{name: "server error", status: http.StatusInternalServerError, want: false},
		{name: "not found", status: http.StatusNotFound, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, "some body bytes to drain")
			}))
			defer srv.Close()

			got, err := Ping(t.Context(), srv.Client(), srv.URL)
			if err != nil {
				t.Fatalf("Ping() err = %v", err)
			}
			if got != tc.want {
				t.Fatalf("Ping() = %t, want %t (status %d)", got, tc.want, tc.status)
			}
		})
	}
}
```

## Review

The suppression is disciplined when the only `//nolint` in the tree names a specific
linter and carries a reason, and `nolintlint` (`require-specific`,
`require-explanation`, `allow-unused: false`) is what turns that from a convention
into an enforced rule. The `Ping` close is the legitimate case: the body is drained,
the close error is not actionable, and the directive documents exactly that so a
reviewer can agree or push back. The mistakes to avoid: a bare `//nolint` that
silences every linter on the line (hides the next bug), a specific directive with no
explanation (unauditable), and a stale directive left behind after the finding it
suppressed is gone (caught only because `allow-unused` is false). The default reaction
to a finding is still to fix the code; `//nolint` is for the genuine exception, and
`nolintlint` keeps the exceptions honest.

## Resources

- [nolintlint](https://golangci-lint.run/docs/linters/configuration/) — `require-explanation`, `require-specific`, `allow-unused` settings.
- [golangci-lint: nolint directive](https://golangci-lint.run/docs/configuration/false-positives/) — the `//nolint` syntax and how it is scoped.
- [errcheck](https://github.com/kisielk/errcheck) — the finding being suppressed here.

---

Back to [07-formatters-section-and-fmt-gate.md](07-formatters-section-and-fmt-gate.md) | Next: [09-scoped-exclusions-tests-and-legacy.md](09-scoped-exclusions-tests-and-legacy.md)
