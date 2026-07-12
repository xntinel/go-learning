# Exercise 5: Merge unit and integration coverage into one gate number

Unit tests cover the branch-heavy logic; integration tests cover the wiring and
serialization only reachable by driving the real process. Neither number alone
reflects what the suite exercised. This module builds a checkout-total package
(unit-tested) and a server that exposes it (integration-driven), then merges the
two coverage tiers with `go tool covdata merge` into one honest whole-suite
profile whose total credits branches reachable through only one tier.

This module is fully self-contained: its own `go mod init`, a demo, and unit tests
on the business logic.

## What you'll build

```text
checkout/                  independent module: example.com/checkout
  go.mod
  total.go                 Total: subtotal, discount tiers, tax; ErrEmptyCart
  total_test.go            unit table tests over the discount/tax branches
  cmd/
    server/
      main.go              instrumented-buildable server exposing POST /total
    demo/
      main.go              runnable demo computing a couple of carts
```

- Files: `total.go`, `total_test.go`, `cmd/server/main.go`, `cmd/demo/main.go`.
- Implement: `Total(items []Item) (int64, error)` in cents with tiered discounts and tax, returning `ErrEmptyCart` for an empty cart, plus a server exposing it over HTTP.
- Test: unit table tests over the discount tiers and the empty-cart branch; the server is driven separately as the integration tier.
- Verify: `go test -count=1 -race ./...`, then the merge workflow below.

### Why two tiers need merging

`Total` carries the interesting branches: a per-tier discount and a tax
calculation. Unit tests drive those directly and cheaply — dozens of input rows,
no HTTP. But the *server* has code the unit tests never touch: JSON decoding, the
error-to-status mapping, the `405` for a wrong method. If you measured only unit
coverage you would miss the handler; if you measured only integration coverage you
would miss the discount-tier rows a unit table exercises far more thoroughly than
a handful of end-to-end requests. The truthful whole-suite number is the *union*
of what both tiers executed, which is what `go tool covdata merge` computes.

The mechanism: `go test` can write raw coverage data (the same format as a
`-cover` binary) into a directory when you point `GOCOVERDIR` at it, and the
integration run already writes such a directory. Merging the two directories and
converting to a text profile yields one `total:` that is at least as high as
either tier and credits every block reached by either.

Create `total.go`:

```go
package checkout

import "errors"

// ErrEmptyCart is returned by Total when there are no items.
var ErrEmptyCart = errors.New("empty cart")

// Item is a line in a cart: unit price in cents and a quantity.
type Item struct {
	PriceCents int64
	Qty        int64
}

// taxBps is the sales tax in basis points (825 = 8.25%).
const taxBps = 825

// Total returns the order total in cents: subtotal, minus a tiered discount,
// plus tax on the discounted subtotal. It returns ErrEmptyCart for no items.
func Total(items []Item) (int64, error) {
	if len(items) == 0 {
		return 0, ErrEmptyCart
	}
	var subtotal int64
	for _, it := range items {
		subtotal += it.PriceCents * it.Qty
	}

	discounted := subtotal - discount(subtotal)
	tax := discounted * taxBps / 10000
	return discounted + tax, nil
}

// discount returns the discount in cents for a subtotal: 0% under $50, 10% from
// $50 to under $200, 15% at $200 and above.
func discount(subtotal int64) int64 {
	switch {
	case subtotal >= 20000:
		return subtotal * 15 / 100
	case subtotal >= 5000:
		return subtotal * 10 / 100
	default:
		return 0
	}
}
```

### The server (the integration target)

The server decodes a cart, calls `Total`, and maps the empty-cart error to a
`400`. This is the code the integration tier covers and the unit tier does not.

Create `cmd/server/main.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"

	"example.com/checkout"
)

func main() {
	ln, err := net.Listen("tcp", envAddr())
	if err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /total", handleTotal)

	srv := &http.Server{Handler: mux}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	go func() { _ = srv.Serve(ln) }()
	log.Printf("listening on %s", ln.Addr())
	<-ctx.Done()
	_ = srv.Shutdown(context.Background())
}

func envAddr() string {
	if a := os.Getenv("ADDR"); a != "" {
		return a
	}
	return "127.0.0.1:8080"
}

func handleTotal(w http.ResponseWriter, r *http.Request) {
	var cart struct {
		Items []checkout.Item `json:"items"`
	}
	if err := json.NewDecoder(r.Body).Decode(&cart); err != nil && err != io.EOF {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	total, err := checkout.Total(cart.Items)
	if err != nil {
		if errors.Is(err, checkout.ErrEmptyCart) {
			http.Error(w, "empty cart", http.StatusBadRequest)
			return
		}
		http.Error(w, "error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]int64{"total_cents": total})
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/checkout"
)

func main() {
	carts := []struct {
		name  string
		items []checkout.Item
	}{
		{"small", []checkout.Item{{PriceCents: 1000, Qty: 2}}},  // $20, no discount
		{"mid", []checkout.Item{{PriceCents: 5000, Qty: 2}}},    // $100, 10% off
		{"large", []checkout.Item{{PriceCents: 10000, Qty: 3}}}, // $300, 15% off
	}
	for _, c := range carts {
		total, err := checkout.Total(c.items)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%s: %d cents\n", c.name, total)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
small: 2165 cents
mid: 9742 cents
large: 27603 cents
```

(`small`: 2000 + 8.25% tax = 2165. `mid`: 10000 - 10% = 9000, +8.25% = 9742.
`large`: 30000 - 15% = 25500, +8.25% = 27603. Integer truncation applies.)

### The unit tests

The unit table drives every discount tier boundary and the empty-cart branch —
the branch-heavy work the server barely exercises.

Create `total_test.go`:

```go
package checkout

import (
	"errors"
	"testing"
)

func TestTotal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		items []Item
		want  int64
	}{
		{"no-discount", []Item{{PriceCents: 1000, Qty: 2}}, 2165},
		{"tier1-boundary", []Item{{PriceCents: 5000, Qty: 1}}, 4871},   // $50 -> 10% off
		{"tier1-mid", []Item{{PriceCents: 5000, Qty: 2}}, 9742},        // $100 -> 10% off
		{"tier2-boundary", []Item{{PriceCents: 20000, Qty: 1}}, 18402}, // $200 -> 15% off
		{"tier2-large", []Item{{PriceCents: 10000, Qty: 3}}, 27603},    // $300 -> 15% off
		{"just-under-tier1", []Item{{PriceCents: 4999, Qty: 1}}, 5411}, // < $50, no discount
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Total(tt.items)
			if err != nil {
				t.Fatalf("Total(%s) unexpected error: %v", tt.name, err)
			}
			if got != tt.want {
				t.Errorf("Total(%s) = %d, want %d", tt.name, got, tt.want)
			}
		})
	}
}

func TestTotalEmptyCart(t *testing.T) {
	t.Parallel()
	if _, err := Total(nil); !errors.Is(err, ErrEmptyCart) {
		t.Fatalf("Total(nil) err = %v, want ErrEmptyCart", err)
	}
}
```

### The merge workflow

Produce a unit-tier coverage directory by running the tests with `GOCOVERDIR`
set, produce an integration-tier directory by driving the `-cover` server, then
merge and convert:

```bash
# 1. Unit tier: go test writes raw covdata when GOCOVERDIR is set.
mkdir -p cov/unit
GOCOVERDIR=$PWD/cov/unit go test -cover ./...

# 2. Integration tier: build an instrumented server and drive it.
go build -cover -o server ./cmd/server
mkdir -p cov/integ
ADDR=127.0.0.1:8090 GOCOVERDIR=$PWD/cov/integ ./server &
sleep 0.3
curl -s -XPOST 127.0.0.1:8090/total -d '{"items":[{"PriceCents":10000,"Qty":3}]}'
curl -s -XPOST 127.0.0.1:8090/total -d '{"items":[]}'   # exercises the empty-cart 400
kill -INT %1        # graceful stop so coverage flushes
wait

# 3. Merge the two tiers into one directory, then a text profile.
mkdir -p cov/merged
go tool covdata merge -i=cov/unit,cov/integ -o=cov/merged
go tool covdata textfmt -i=cov/merged -o=combined.txt
go tool cover -func=combined.txt | tail -1
```

Expected output (abbreviated):

```
total:  (statements)   96.0%
```

The merged total is at least as high as either tier alone: the unit tier drove
`discount`'s tiers exhaustively, the integration tier drove `handleTotal`'s JSON
decode and the empty-cart `400` that no unit test hits. Neither number alone is
honest; the merge is. Compare `go tool covdata percent -i=cov/unit` and
`-i=cov/integ` individually to see each tier below the merged figure.

## Review

The logic is correct when `Total` applies 0/10/15% discounts at the $50 and $200
boundaries, adds 8.25% tax on the discounted subtotal with integer truncation, and
returns `ErrEmptyCart` for no items — pinned by the unit table and the empty-cart
test. The merge lesson is correct when `go tool covdata merge` of the unit and
integration directories yields a `total:` no lower than either tier, crediting the
handler blocks only the integration run reached and the discount tiers the unit
run covered densely.

The mistake to avoid is reporting one tier as "the coverage" of a service that has
both. Merge them: `go test` with `GOCOVERDIR` for the unit tier, a `-cover` binary
for the integration tier, `covdata merge` then `textfmt`, and read the combined
`total:`. Remember `covdata` reads raw data directories while `go tool cover`
reads the converted text profile — do not cross the two. Run `go test -race ./...`
to keep the logic clean.

## Resources

- [Code coverage for Go integration tests](https://go.dev/blog/integration-test-coverage) — writing covdata from `go test` and from a binary, and merging.
- [`go tool covdata`](https://pkg.go.dev/cmd/covdata) — `merge`, `textfmt`, `percent`.
- [`go tool cover`](https://pkg.go.dev/cmd/cover) — reading the merged text profile.

---

Back to [04-integration-coverage-server-binary.md](04-integration-coverage-server-binary.md) | Next: [06-coverage-threshold-ci-gate.md](06-coverage-threshold-ci-gate.md)
