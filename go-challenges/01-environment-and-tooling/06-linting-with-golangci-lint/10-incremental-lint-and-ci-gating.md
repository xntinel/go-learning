# Exercise 10: Incremental Linting and the CI Gate

The capstone is the real adoption problem: you inherit a service that has never been
linted and has thousands of latent findings, and you must make the linter a required
CI gate *without* a big-bang cleanup. The technique is `--new-from-merge-base`, which
gates only newly changed lines against a git baseline. This exercise builds a small
HTTP health service, wires it as the thing CI lints, and walks the incremental gate,
version pinning, and cache mechanics that make the gate cheap and reproducible.

This module is self-contained: its own `go mod init`, a `health` HTTP handler, a
demo, and an `httptest` table test.

## What you'll build

```text
healthgate/                   independent module: example.com/healthgate
  go.mod                      go 1.24
  health.go                   Handler() http.Handler serving /healthz + /readyz
  health_test.go              httptest table over the routes
  cmd/
    demo/
      main.go                 serves the handler via httptest and probes it
  .golangci.yml               the gate config (shown in prose)
  .github/workflows/lint.yml  pinned CI gate (shown in prose)
```

- Files: `health.go`, `health_test.go`, `cmd/demo/main.go`, plus the config and CI workflow in prose.
- Implement: `Handler()` returning an `http.Handler` that serves `/healthz` (always 200) and `/readyz` (200 or 503 based on a readiness flag).
- Test: an `httptest.Server` table asserting status and body per route.
- Verify: `go test -count=1 -race ./...`; then the incremental-gate walkthrough.

### How the incremental gate works

`golangci-lint run --new-from-merge-base=main` computes the merge base between your
branch and `main`, diffs against it, and reports only findings on lines that your
branch *added or changed*. A pre-existing finding on an untouched line is skipped.
This is what lets you require a clean gate on new code immediately: a PR must not
*introduce* a finding, but it is not blocked by the ten thousand findings already in
the tree. `--new-from-rev <sha>` is the same idea against an explicit revision, and
the config keys `issues.new-from-merge-base` / `issues.new-from-rev` bake the baseline
into the file so every run uses it.

The mechanics worth internalizing: the baseline is a git reference, so the diff is
line-based, and *moving* an old finding onto a changed line makes it "new" again —
which is a feature, because touching bad code becomes an opportunity to fix it. Run
plain `golangci-lint run ./...` (no `--new-from-*`) to see the full backlog you are
burning down separately; the two modes answer different questions ("is this PR
clean?" versus "how much legacy debt remains?").

Concretely, on a repo with a committed pre-existing finding plus a new file that
adds its own finding:

```bash
# only the finding your branch introduced:
golangci-lint run --new-from-merge-base=main ./...

# the whole backlog, old and new:
golangci-lint run ./...
```

The first reports one finding (the new one); the second reports both.

### Reproducibility: pin the version, warm the cache

"Passes locally, fails in CI" is almost always a version skew: the default linter set
and individual analyzers change between releases, so an unpinned tool produces
different findings on different machines. Pin it — either `go install
github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.1.6` or the official CI
action at a fixed version — and commit `.golangci.yml`, so every laptop and runner
resolve the same binary and ruleset. `golangci-lint` keeps a build/analysis cache
(`golangci-lint cache status` shows it, `cache clean` empties it) so warm runs reuse
type-checking results and finish in seconds; CI caches that directory between runs to
keep the gate cheap.

Create `health.go`:

```go
package health

import (
	"net/http"
	"sync/atomic"
)

// Handler serves liveness and readiness endpoints. /healthz is always 200 once
// the process is up; /readyz reflects the ready flag so a load balancer can drain
// the instance before shutdown.
func Handler(ready *atomic.Bool) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("not ready"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})

	return mux
}
```

### The config and the CI gate

Create `.golangci.yml`:

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

Create `.github/workflows/lint.yml` — a required check pinned to an exact version, so
CI is reproducible and gates new code:

```yaml
name: lint
on:
  pull_request:
permissions:
  contents: read
jobs:
  golangci:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0   # full history so merge-base can be computed
      - uses: actions/setup-go@v5
        with:
          go-version: "1.24"
      - uses: golangci/golangci-lint-action@v6
        with:
          version: v2.1.6
          args: --new-from-merge-base=origin/main ./...
```

`fetch-depth: 0` matters: without full history the runner cannot compute the merge
base, and `--new-from-merge-base` silently degrades. Pinning both the Go version and
the linter version is what makes the CI verdict match a developer's local run.

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"sync/atomic"

	"example.com/healthgate"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	var ready atomic.Bool
	srv := httptest.NewServer(health.Handler(&ready))
	defer srv.Close()

	probe := func(path string) error {
		resp, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		fmt.Printf("%s -> %d %s\n", path, resp.StatusCode, body)
		return nil
	}

	if err := probe("/healthz"); err != nil {
		return err
	}
	if err := probe("/readyz"); err != nil { // not ready yet
		return err
	}
	ready.Store(true)
	return probe("/readyz") // now ready
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
/healthz -> 200 ok
/readyz -> 503 not ready
/readyz -> 200 ready
```

### Tests

Create `health_test.go`. The table drives each route through an `httptest.Server`,
flipping the readiness flag to cover both `/readyz` branches, and closes every
response body (so the suite itself would pass `bodyclose`).

```go
package health

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestHandler(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		path       string
		ready      bool
		wantStatus int
		wantBody   string
	}{
		{name: "healthz", path: "/healthz", ready: false, wantStatus: http.StatusOK, wantBody: "ok"},
		{name: "readyz not ready", path: "/readyz", ready: false, wantStatus: http.StatusServiceUnavailable, wantBody: "not ready"},
		{name: "readyz ready", path: "/readyz", ready: true, wantStatus: http.StatusOK, wantBody: "ready"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var ready atomic.Bool
			ready.Store(tc.ready)
			srv := httptest.NewServer(Handler(&ready))
			defer srv.Close()

			resp, err := srv.Client().Get(srv.URL + tc.path)
			if err != nil {
				t.Fatalf("GET %s err = %v", tc.path, err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("GET %s status = %d, want %d", tc.path, resp.StatusCode, tc.wantStatus)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body err = %v", err)
			}
			if string(body) != tc.wantBody {
				t.Fatalf("GET %s body = %q, want %q", tc.path, body, tc.wantBody)
			}
		})
	}
}
```

## Review

The gate is adopted correctly when a PR's *new* findings block the merge while the
legacy backlog does not — that is what `--new-from-merge-base=origin/main` buys, and
`fetch-depth: 0` is the non-obvious prerequisite for it to work in CI. The health
handler is the code under the gate; the deliverables are the incremental invocation,
the pinned versions (Go and linter), and the committed config that together make the
CI verdict identical to a local run. The mistakes to avoid: big-bang adoption that
tries to clear thousands of findings in one PR (use the incremental gate instead),
an unpinned linter that drifts CI away from laptops, a shallow checkout that makes
merge-base computation silently fall back to full mode, and running the linter only
locally so the gate is advisory rather than required. Run `go test -race ./...` to
confirm the handler, then treat `golangci-lint run --new-from-merge-base` as the
check that actually decides whether the PR merges.

## Resources

- [golangci-lint: Command-Line (run flags)](https://golangci-lint.run/docs/configuration/cli/) — `--new-from-merge-base`, `--new-from-rev`, `--timeout`.
- [golangci-lint-action](https://github.com/golangci/golangci-lint-action) — the pinned GitHub Actions integration.
- [golangci-lint: Configuration File (issues)](https://golangci-lint.run/docs/configuration/file/) — `issues.new-from-merge-base` / `new-from-rev`.
- [`net/http`: ServeMux method patterns](https://pkg.go.dev/net/http#ServeMux) — the `GET /path` routing used by the handler.

---

Back to [09-scoped-exclusions-tests-and-legacy.md](09-scoped-exclusions-tests-and-legacy.md) | Next: [../07-debugging-with-delve/00-concepts.md](../07-debugging-with-delve/00-concepts.md)
