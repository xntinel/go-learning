# Exercise 17: Route Health Checks to Type-Specific Executors

**Nivel: Intermedio** — validacion rapida (un test corto).

A backend health checker runs three different kinds of probes against
dependencies: a raw TCP dial against a database, an HTTP GET against a
sidecar's `/healthz`, and a gRPC health-check ping against another service.
Each probe kind carries different required fields, a different success
criterion, and a different default timeout, so the router that dispatches a
decoded probe config to its executor needs to know the probe's concrete
type before it can even validate the config, let alone run it.

## What you'll build

```text
health-check-router/        independent module: example.com/health-check-router
  go.mod                     go 1.24
  healthrouter.go            Run(probe any) (Result, error)
  cmd/
    demo/
      main.go                runs one of each probe kind against injected fakes
  healthrouter_test.go        table test over every probe kind plus invalid configs
```

- Files: `healthrouter.go`, `cmd/demo/main.go`, `healthrouter_test.go`.
- Implement: `Run(probe any) (Result, error)`, type-switching on `TCPProbe`,
  `HTTPProbe`, and `GRPCProbe`, each with an injected check function so no
  test or demo run ever touches a real network.
- Test: each probe kind succeeding and failing its check, each probe kind
  missing its required field, and an unsupported probe type.

Set up the module:

```bash
mkdir -p ~/go-exercises/health-check-router/cmd/demo
cd ~/go-exercises/health-check-router
go mod init example.com/health-check-router
go mod edit -go=1.24
```

Every probe kind injects its own check function (`Dial`, `Get`, `Ping`)
instead of the router reaching for `net.Dial` or `http.Get` directly, which
is what makes both the tests and the demo deterministic — a real health
checker would wire these to the standard library, but the router's own
logic (which field is required, what counts as a default timeout, what
counts as success) is what this exercise drills, not network I/O. Each
probe's zero-value `Timeout` falling back to `defaultTimeout` mirrors a real
health checker's behavior: an operator who forgets to set a timeout should
get a sane default, not an instantly-expiring context. The HTTP case
treats any non-2xx status as a failed `Result`, not an error — a `503` from
a real dependency is exactly the outcome the health checker exists to
detect, so it must be a normal `Result{OK: false}`, while a missing URL
scheme is a configuration mistake and returns an error instead, since no
retry or alerting logic should ever see a probe that was never valid to
begin with.

Create `healthrouter.go`:

```go
package healthrouter

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrInvalidProbe is the sentinel for a probe whose configuration is
// incomplete, and ErrUnsupportedProbe is the sentinel for a probe type the
// router has no executor for.
var (
	ErrInvalidProbe     = errors.New("invalid probe configuration")
	ErrUnsupportedProbe = errors.New("unsupported probe type")
)

const defaultTimeout = 2 * time.Second

// TCPProbe checks that a raw socket can be opened to Addr. Dial is injected
// so tests never touch a real network.
type TCPProbe struct {
	Addr    string
	Timeout time.Duration
	Dial    func(addr string, timeout time.Duration) error
}

// HTTPProbe checks that an HTTP endpoint responds. Get is injected for the
// same reason.
type HTTPProbe struct {
	URL     string
	Timeout time.Duration
	Get     func(url string, timeout time.Duration) (status int, err error)
}

// GRPCProbe checks that a gRPC service answers a health ping. Ping is
// injected likewise.
type GRPCProbe struct {
	Service string
	Timeout time.Duration
	Ping    func(service string, timeout time.Duration) error
}

// Result is the outcome of running one probe, normalized across probe kinds
// so a caller can log or aggregate them uniformly.
type Result struct {
	Kind string
	OK   bool
}

// Run dispatches a probe to its executor by concrete type. Each probe kind
// has different required fields and a different success criterion (a dial
// error, a non-2xx status, an RPC error), so the executor lives next to the
// type switch instead of behind a shared interface: the type set is fixed
// and the router is the one place that changes when a probe's validation
// rule changes.
func Run(probe any) (Result, error) {
	switch p := probe.(type) {
	case TCPProbe:
		if p.Addr == "" {
			return Result{}, fmt.Errorf("%w: tcp probe missing addr", ErrInvalidProbe)
		}
		timeout := p.Timeout
		if timeout <= 0 {
			timeout = defaultTimeout
		}
		if err := p.Dial(p.Addr, timeout); err != nil {
			return Result{Kind: "tcp", OK: false}, nil
		}
		return Result{Kind: "tcp", OK: true}, nil
	case HTTPProbe:
		if !strings.HasPrefix(p.URL, "http://") && !strings.HasPrefix(p.URL, "https://") {
			return Result{}, fmt.Errorf("%w: http probe url %q missing scheme", ErrInvalidProbe, p.URL)
		}
		timeout := p.Timeout
		if timeout <= 0 {
			timeout = defaultTimeout
		}
		status, err := p.Get(p.URL, timeout)
		if err != nil || status < 200 || status >= 300 {
			return Result{Kind: "http", OK: false}, nil
		}
		return Result{Kind: "http", OK: true}, nil
	case GRPCProbe:
		if p.Service == "" {
			return Result{}, fmt.Errorf("%w: grpc probe missing service", ErrInvalidProbe)
		}
		timeout := p.Timeout
		if timeout <= 0 {
			timeout = defaultTimeout
		}
		if err := p.Ping(p.Service, timeout); err != nil {
			return Result{Kind: "grpc", OK: false}, nil
		}
		return Result{Kind: "grpc", OK: true}, nil
	default:
		return Result{}, fmt.Errorf("%w: %T", ErrUnsupportedProbe, probe)
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"time"

	"example.com/health-check-router"
)

func main() {
	probes := []any{
		healthrouter.TCPProbe{
			Addr: "db:5432",
			Dial: func(addr string, timeout time.Duration) error { return nil },
		},
		healthrouter.HTTPProbe{
			URL: "http://svc/healthz",
			Get: func(url string, timeout time.Duration) (int, error) { return 503, nil },
		},
		healthrouter.GRPCProbe{
			Service: "billing.v1.Billing",
			Ping: func(service string, timeout time.Duration) error {
				return errors.New("unreachable")
			},
		},
	}
	for _, p := range probes {
		result, err := healthrouter.Run(p)
		if err != nil {
			fmt.Printf("invalid: %v\n", err)
			continue
		}
		fmt.Printf("%s: ok=%v\n", result.Kind, result.OK)
	}
}
```

```bash
go run ./cmd/demo
```

Expected output:

```text
tcp: ok=true
http: ok=false
grpc: ok=false
```

### Tests

Create `healthrouter_test.go`:

```go
package healthrouter

import (
	"errors"
	"testing"
	"time"
)

func TestRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		probe   any
		want    Result
		wantErr error
	}{
		{
			name: "tcp probe succeeds",
			probe: TCPProbe{
				Addr: "db:5432",
				Dial: func(addr string, timeout time.Duration) error { return nil },
			},
			want: Result{Kind: "tcp", OK: true},
		},
		{
			name: "tcp probe dial failure is a failed result, not an error",
			probe: TCPProbe{
				Addr: "db:5432",
				Dial: func(addr string, timeout time.Duration) error { return errors.New("refused") },
			},
			want: Result{Kind: "tcp", OK: false},
		},
		{
			name:    "tcp probe missing addr is invalid",
			probe:   TCPProbe{Dial: func(string, time.Duration) error { return nil }},
			wantErr: ErrInvalidProbe,
		},
		{
			name: "http probe succeeds on 2xx",
			probe: HTTPProbe{
				URL: "http://svc/healthz",
				Get: func(url string, timeout time.Duration) (int, error) { return 200, nil },
			},
			want: Result{Kind: "http", OK: true},
		},
		{
			name: "http probe fails on non-2xx",
			probe: HTTPProbe{
				URL: "http://svc/healthz",
				Get: func(url string, timeout time.Duration) (int, error) { return 503, nil },
			},
			want: Result{Kind: "http", OK: false},
		},
		{
			name:    "http probe missing scheme is invalid",
			probe:   HTTPProbe{URL: "svc/healthz", Get: func(string, time.Duration) (int, error) { return 200, nil }},
			wantErr: ErrInvalidProbe,
		},
		{
			name: "grpc probe succeeds",
			probe: GRPCProbe{
				Service: "billing.v1.Billing",
				Ping:    func(service string, timeout time.Duration) error { return nil },
			},
			want: Result{Kind: "grpc", OK: true},
		},
		{
			name:    "grpc probe missing service is invalid",
			probe:   GRPCProbe{Ping: func(string, time.Duration) error { return nil }},
			wantErr: ErrInvalidProbe,
		},
		{
			name:    "unsupported probe type",
			probe:   "not-a-probe",
			wantErr: ErrUnsupportedProbe,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Run(tt.probe)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Run(%v) err = %v, want %v", tt.probe, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Run(%v) unexpected error: %v", tt.probe, err)
			}
			if got != tt.want {
				t.Fatalf("Run(%v) = %+v, want %+v", tt.probe, got, tt.want)
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

Each case validates its own required field before invoking its injected
check function, so a probe missing `Addr`, `URL`, or `Service` fails fast
with `ErrInvalidProbe` instead of calling a check function with an empty
target. The timeout fallback (`if timeout <= 0`) applies uniformly across
all three cases, which is deliberate: a health checker with an
accidentally-zero timeout for one probe kind but not another is a subtle
production bug this structure avoids by repeating the same one-line rule in
each branch rather than trusting every caller to set `Timeout` explicitly.
The most common way to break this router is to conflate "probe failed its
check" with "probe was misconfigured" — collapsing both into the same error
path would make a legitimate `503` from a struggling dependency
indistinguishable from an operator's typo in a config file.

## Resources

- [Go Specification: Type switches](https://go.dev/ref/spec#Type_switches)
- [gRPC Health Checking Protocol](https://github.com/grpc/grpc/blob/master/doc/health-checking.md)
- [Kubernetes: Configure Liveness, Readiness and Startup Probes](https://kubernetes.io/docs/tasks/configure-pod-container/configure-liveness-readiness-startup-probes/)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [16-batch-request-unpacker.md](16-batch-request-unpacker.md) | Next: [18-oauth-grant-handler.md](18-oauth-grant-handler.md)
