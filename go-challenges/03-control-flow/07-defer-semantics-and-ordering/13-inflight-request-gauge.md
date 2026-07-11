# Exercise 13: In-Flight Request Gauge — Deferred Decrement Across Exit Paths

**Nivel: Intermedio** — validacion rapida (un test corto).

An in-flight request gauge that feeds a dashboard is only trustworthy if it
decrements on every single exit path a handler has — the early validation
error, the mid-function business error, and the success return alike. This
module builds a handler with three distinct return statements and proves one
`defer` right after the increment covers all three, with no repeated
decrement call to forget.

## What you'll build

```text
gauge/                      independent module: example.com/inflight-request-gauge
  go.mod                     go 1.24
  gauge.go                   Gauge (Inc/Dec/Value); Handle(g, input) (string, error)
  gauge_test.go              table test over validation, business, success paths
```

- Files: `gauge.go`, `gauge_test.go`.
- Implement: `Gauge` with `Inc`, `Dec`, `Value` methods, and `Handle(g *Gauge, input string) (string, error)` with three exit paths.
- Test: a table over inputs that hit each exit path, asserting the return value and that `g.Value()` is back to `0` after every call.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/gauge
cd ~/go-exercises/gauge
go mod init example.com/inflight-request-gauge
go mod edit -go=1.24
```

### One defer, three return statements

`Handle` has three ways to leave: an early return when `input` fails
validation, a return partway through when a business rule rejects the
request, and the normal success return at the end. A handler shaped this way
is exactly what tempts someone into writing `g.Dec()` by hand at each return
site — and exactly the shape where one of those three copies eventually gets
forgotten when a fourth branch is added later, leaving the gauge permanently
elevated by one for every request that took the forgotten path. `defer
g.Dec()`, registered once immediately after `g.Inc()`, removes the choice
entirely: it is not possible to add a new return statement to this function
without the decrement also firing on it.

Create `gauge.go`:

```go
package gauge

import "errors"

// ErrInvalid means the request failed input validation.
var ErrInvalid = errors.New("invalid request")

// ErrBusiness means the request failed a business rule after validation.
var ErrBusiness = errors.New("business rule failed")

// Gauge tracks the number of requests currently being handled. Production
// code exposes Value as a Prometheus gauge metric; here it is a plain
// counter so the test can assert on it directly with no metrics backend.
type Gauge struct {
	value int
}

// Inc increments the in-flight count.
func (g *Gauge) Inc() { g.value++ }

// Dec decrements the in-flight count.
func (g *Gauge) Dec() { g.value-- }

// Value returns the current in-flight count.
func (g *Gauge) Value() int { return g.value }

// Handle processes one request, tracking it on g for the duration. The
// defer registered immediately after Inc guarantees Dec runs on every exit
// path below — the early validation-error return, the business-error
// return, and the success return — without repeating g.Dec() at each one.
// A handler with three return statements and no defer is exactly the shape
// that tempts someone into forgetting the decrement on one of the paths.
func Handle(g *Gauge, input string) (string, error) {
	g.Inc()
	defer g.Dec()

	if input == "" {
		return "", ErrInvalid
	}

	if input == "reject" {
		return "", ErrBusiness
	}

	return "processed:" + input, nil
}
```

### Test

`TestHandle` runs `Handle` once per exit path and checks both the returned
value and that the gauge is back at `0` afterward — reusing the same `Gauge`
across cases to also demonstrate it never drifts.

Create `gauge_test.go`:

```go
package gauge

import "testing"

func TestHandle(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantOut string
		wantErr error
	}{
		{"empty input is invalid", "", "", ErrInvalid},
		{"reject triggers a business error", "reject", "", ErrBusiness},
		{"valid input succeeds", "order-1", "processed:order-1", nil},
	}

	g := &Gauge{}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			out, err := Handle(g, tc.input)

			if out != tc.wantOut {
				t.Errorf("out = %q, want %q", out, tc.wantOut)
			}
			if err != tc.wantErr {
				t.Errorf("err = %v, want %v", err, tc.wantErr)
			}
			if g.Value() != 0 {
				t.Errorf("gauge value after Handle = %d, want 0", g.Value())
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

The property under test is not any single return value — it is that the
gauge always settles back to zero, regardless of which of the three exits
`Handle` took to get there. That is the entire value proposition of
`defer` for a resource-tracking counter: the release logic lives in exactly
one place, next to the acquisition, and is structurally incapable of being
skipped by a return statement anywhere below it, including ones that do not
exist yet.

## Resources

- [Go Specification: Defer statements](https://go.dev/ref/spec#Defer_statements) — a deferred function's execution is guaranteed on every return path.
- [Prometheus: Gauge metric type](https://prometheus.io/docs/concepts/metric_types/#gauge) — the production shape this in-flight counter mirrors.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-nested-savepoint-release-order.md](12-nested-savepoint-release-order.md) | Next: [14-audit-event-pointer-capture.md](14-audit-event-pointer-capture.md)
