# Exercise 8: A Separate e2e Tier With A Second Build Tag

The integration tier owns the database; a third tier — end-to-end — drives a
*running service* over HTTP. This module builds that tier behind `//go:build e2e`,
kept distinct from `integration`, and shows the CI matrix where `go test ./...`,
`-tags=integration`, and `-tags=e2e` are three independent stages. It also
demonstrates a boolean tag expression, `//go:build integration && !race`, that
excludes a file under the race detector.

Self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
e2eclient/                 independent module: example.com/e2eclient
  go.mod
  client.go                HealthCheck, FetchAccount over http.Client; wrapped ErrNotFound
  cmd/
    demo/
      main.go              drives the client against an in-process httptest server
  client_test.go           httptest-backed default-build tests + Example
  e2e_test.go              //go:build e2e: drives E2E_BASE_URL, skips when unset
  slow_integration_test.go //go:build integration && !race: excluded under -race
```

- Files: `client.go`, `cmd/demo/main.go`, `client_test.go`, `e2e_test.go`, `slow_integration_test.go`.
- Implement: an HTTP client with `HealthCheck` and `FetchAccount` (wrapped `ErrNotFound` on 404).
- Test: the client against `httptest.Server` in the default build; the e2e tier against `E2E_BASE_URL`; a boolean-tagged file excluded under `-race`.
- Verify: `go test ./...` runs none of the tagged tiers; `-tags=integration` and `-tags=e2e` run their own; the `&& !race` file is excluded when `-race` is set.

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/18-integration-tests-with-build-tags/08-e2e-tier-second-build-tag/cmd/demo
cd go-solutions/12-testing-ecosystem/18-integration-tests-with-build-tags/08-e2e-tier-second-build-tag
```

### Three tiers, three tags, three stages

Integration and e2e are different jobs and deserve different tags. The integration
tier owns *state* — it talks to a database it migrates and seeds. The e2e tier owns
*a running system* — it makes real HTTP requests to a service already deployed at a
base URL, exercising routing, serialization, and status codes end to end, with the
database as an opaque dependency behind the service. They fail for different reasons
and run in different CI stages with different environment: `DATABASE_URL` for
integration, `E2E_BASE_URL` for e2e. Keeping them under separate tags means a broken
service does not fail the database stage and vice versa, and each stage can carry its
own `-race`, `-count`, and timeout policy.

The client itself is default-build code: `HealthCheck` and `FetchAccount` take an
`*http.Client` and a base URL, so the fast tier tests them against
`httptest.NewServer` — real HTTP over a loopback socket, no external service — and
the e2e tier points the same functions at `E2E_BASE_URL`. `t.Context()` bounds every
request so a hung service fails the test instead of the stage.

### The boolean tag expression

`//go:build integration && !race` compiles a file in the integration tier but never
under the race detector. This is the real-world escape hatch for a test that is too
slow under `-race`, or that exercises a dependency that is not race-clean: the file
simply drops out when `race` is set (the toolchain sets the `race` tag
automatically under `-race`). One file targets a precise slice of the matrix without
a second copy.

Create `client.go`:

```go
package e2eclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// ErrNotFound is returned (wrapped) when the service answers 404.
var ErrNotFound = errors.New("e2eclient: resource not found")

// Account is the JSON the service returns for an account.
type Account struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// HealthCheck GETs {baseURL}/healthz and returns the status code.
func HealthCheck(ctx context.Context, client *http.Client, baseURL string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/healthz", nil)
	if err != nil {
		return 0, fmt.Errorf("build health request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("do health request: %w", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// FetchAccount GETs {baseURL}/accounts/{id}, decoding the JSON body. A 404 maps to
// a wrapped ErrNotFound; any other non-2xx is an error.
func FetchAccount(ctx context.Context, client *http.Client, baseURL, id string) (Account, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/accounts/"+id, nil)
	if err != nil {
		return Account{}, fmt.Errorf("build account request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return Account{}, fmt.Errorf("do account request: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		var a Account
		if err := json.NewDecoder(resp.Body).Decode(&a); err != nil {
			return Account{}, fmt.Errorf("decode account: %w", err)
		}
		return a, nil
	case http.StatusNotFound:
		return Account{}, fmt.Errorf("fetch %q: %w", id, ErrNotFound)
	default:
		return Account{}, fmt.Errorf("fetch %q: unexpected status %d", id, resp.StatusCode)
	}
}
```

Now the e2e tier drives a real running service:

Create `e2e_test.go`:

```go
//go:build e2e

package e2eclient

import (
	"net/http"
	"os"
	"testing"
	"time"
)

func TestHealthE2E(t *testing.T) {
	base := os.Getenv("E2E_BASE_URL")
	if base == "" {
		t.Skip("e2e tier: set E2E_BASE_URL to the running service to run")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	code, err := HealthCheck(t.Context(), client, base)
	if err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	if code != http.StatusOK && code != http.StatusNoContent {
		t.Fatalf("health status = %d, want 200 or 204", code)
	}
}
```

And the boolean-tagged integration file, excluded under `-race`:

Create `slow_integration_test.go`:

```go
//go:build integration && !race

package e2eclient

import "testing"

// TestSlowScan stands in for an integration test too slow to run under the race
// detector. The `&& !race` constraint drops this file when -race is set, so the
// integration stage can run it plain and skip it under -race from one file.
func TestSlowScan(t *testing.T) {
	t.Log("slow integration scan; excluded from -race builds by the tag expression")
}
```

### The runnable demo

The demo starts an in-process `httptest` server and drives the client against it, so
it runs with no external service.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/e2eclient"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/accounts/acct:1", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"acct:1","name":"alice"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx := context.Background()
	code, _ := e2eclient.HealthCheck(ctx, srv.Client(), srv.URL)
	fmt.Printf("health -> %d\n", code)

	acct, _ := e2eclient.FetchAccount(ctx, srv.Client(), srv.URL, "acct:1")
	fmt.Printf("account -> %s (%s)\n", acct.ID, acct.Name)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
health -> 204
account -> acct:1 (alice)
```

### Tests

The default-build tests drive the client against `httptest.NewServer` — real HTTP,
no external service — covering the health path, the JSON decode, and the 404 →
`ErrNotFound` mapping.

Create `client_test.go`:

```go
package e2eclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestServer() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/accounts/acct:1", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"acct:1","name":"alice"}`)
	})
	mux.HandleFunc("/accounts/acct:404", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	return httptest.NewServer(mux)
}

func TestHealthCheck(t *testing.T) {
	t.Parallel()
	srv := newTestServer()
	t.Cleanup(srv.Close)

	code, err := HealthCheck(t.Context(), srv.Client(), srv.URL)
	if err != nil || code != http.StatusNoContent {
		t.Fatalf("HealthCheck = %d, %v; want 204, nil", code, err)
	}
}

func TestFetchAccount(t *testing.T) {
	t.Parallel()
	srv := newTestServer()
	t.Cleanup(srv.Close)

	acct, err := FetchAccount(t.Context(), srv.Client(), srv.URL, "acct:1")
	if err != nil {
		t.Fatalf("FetchAccount: %v", err)
	}
	if acct.ID != "acct:1" || acct.Name != "alice" {
		t.Fatalf("account = %+v, want acct:1/alice", acct)
	}
}

func TestFetchAccountNotFound(t *testing.T) {
	t.Parallel()
	srv := newTestServer()
	t.Cleanup(srv.Close)

	_, err := FetchAccount(t.Context(), srv.Client(), srv.URL, "acct:404")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("FetchAccount(404) = %v, want wrapped ErrNotFound", err)
	}
}

func ExampleHealthCheck() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	code, _ := HealthCheck(context.Background(), srv.Client(), srv.URL)
	fmt.Println(code)
	// Output: 200
}
```

## Review

The e2e tier is a distinct concern from the integration tier: it drives a running
service over HTTP, owns `E2E_BASE_URL`, and fails for different reasons than the
database tier — so it gets its own tag and its own CI stage. The client is
default-build code tested against `httptest`, which is why the fast tier can pin the
health, decode, and 404 paths with no external service; the same functions run in
the e2e stage against the real base URL. The boolean expression
`//go:build integration && !race` shows how one file targets a precise slice of the
matrix — present in the integration tier, absent under `-race` — without a second
copy. Confirm `go test ./...` runs only the `httptest` tests, `-tags=e2e` adds
`TestHealthE2E` (skipping without `E2E_BASE_URL`), and `-tags=integration` without
`-race` compiles `slow_integration_test.go` while `-tags=integration -race` excludes
it.

## Resources

- [net/http: NewRequestWithContext](https://pkg.go.dev/net/http#NewRequestWithContext) — building a context-bound request.
- [net/http/httptest: NewServer](https://pkg.go.dev/net/http/httptest#NewServer) — the in-process server the default tests drive.
- [go command: Build constraints](https://pkg.go.dev/cmd/go#hdr-Build_constraints) — boolean tag expressions and the automatic `race` tag.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-schema-migrate-and-seed.md](07-schema-migrate-and-seed.md) | Next: [09-vet-and-coverage-across-tags.md](09-vet-and-coverage-across-tags.md)
