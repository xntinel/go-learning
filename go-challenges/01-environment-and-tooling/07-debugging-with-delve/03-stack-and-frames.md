# Exercise 3: Walk the Stack — Frames, args, and up/down

When a bad value surfaces deep in a call chain, the question is rarely "what is
wrong here" but "how did this value get here". Delve answers it by letting you
walk the stack: read the full trace, switch to a caller's frame, and print the
arguments and locals that were in scope when the caller made the call. This module
builds a handler → service → repo chain and reconstructs how an id travelled down
it.

This module is fully self-contained: its own `go mod init`, demo, and test.

## What you'll build

```text
callchain/                 independent module: example.com/callchain
  go.mod                   go 1.24
  orders/
    orders.go              Repo.Find, Service.Fetch, Handler.Describe; ErrNotFound
  cmd/
    demo/
      main.go              drives the chain for a present and a missing id
  orders/orders_test.go    table-driven test; errors.Is on the sentinel
```

- Files: `orders/orders.go`, `cmd/demo/main.go`, `orders/orders_test.go`.
- Implement: a three-layer chain `Handler.Describe` → `Service.Fetch` → `Repo.Find`, with `ErrNotFound` wrapped by the service.
- Test: assert the description for a present id and `errors.Is(err, ErrNotFound)` for a missing one.
- Verify: `go test -count=1 -race ./...`, then a scripted `dlv` session that reads the stack and a caller frame's variable.

Set up the module:

```bash
mkdir -p ~/go-exercises/callchain/orders ~/go-exercises/callchain/cmd/demo
cd ~/go-exercises/callchain
go mod init example.com/callchain
go mod edit -go=1.24
```

### Frames, args, and the up/down cursor

When Delve stops, it has a frame cursor pointing at the innermost frame (frame 0).
`stack` (alias `bt`, for backtrace) prints the frames innermost-first: frame 0 is
where you stopped, frame 1 is its caller, and so on out to `runtime.main`.
`frame <n>` moves the cursor to frame n, and `up`/`down` move it one frame toward
the caller/callee. Once the cursor is on a frame, `args` prints that frame's
function arguments, `locals` prints its local variables, and `print <expr>`
evaluates in that frame's scope. This is the mechanism for answering "what id did
the service pass down": stop in the repo, move up to the service's frame, and
print the id it held.

Create `orders/orders.go`:

```go
package orders

import (
	"errors"
	"fmt"
)

// ErrNotFound is returned (wrapped) when no order exists for an id.
var ErrNotFound = errors.New("order not found")

// Order is a stored order.
type Order struct {
	ID    int
	Total int
}

// Repo is the innermost layer: it owns the data.
type Repo struct {
	byID map[int]Order
}

func NewRepo(orders ...Order) *Repo {
	m := make(map[int]Order, len(orders))
	for _, o := range orders {
		m[o.ID] = o
	}
	return &Repo{byID: m}
}

// Find looks up an order by id. It returns (zero, false) when absent.
func (r *Repo) Find(id int) (Order, bool) {
	o, ok := r.byID[id]
	return o, ok
}

// Service sits above the repo and translates absence into an error.
type Service struct {
	repo *Repo
}

func NewService(repo *Repo) *Service {
	return &Service{repo: repo}
}

// Fetch returns the order or an error wrapping ErrNotFound.
func (s *Service) Fetch(id int) (Order, error) {
	o, ok := s.repo.Find(id)
	if !ok {
		return Order{}, fmt.Errorf("fetch order %d: %w", id, ErrNotFound)
	}
	return o, nil
}

// Handler is the outermost layer: it renders a human-readable line.
type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// Describe returns a one-line description or an error from the chain below it.
func (h *Handler) Describe(id int) (string, error) {
	o, err := h.svc.Fetch(id)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("order %d total %d", o.ID, o.Total), nil
}
```

### Reading the chain under Delve

Break in the innermost layer, run to it, and read the stack. The frames confirm
the path the call took; switching up a frame reveals what the caller was holding:

```bash
dlv debug ./cmd/demo
```

```text
(dlv) break example.com/callchain/orders.(*Repo).Find
Breakpoint 1 set at 0x... for example.com/callchain/orders.(*Repo).Find() ./orders/orders.go:32
(dlv) continue
> example.com/callchain/orders.(*Repo).Find() ./orders/orders.go:32 (hits goroutine(1):1 total:1)
(dlv) stack
0  example.com/callchain/orders.(*Repo).Find() ./orders/orders.go:32
1  example.com/callchain/orders.(*Service).Fetch() ./orders/orders.go:47
2  example.com/callchain/orders.(*Handler).Describe() ./orders/orders.go:65
3  main.main() ./cmd/demo/main.go:14
...
(dlv) args
r = ("*example.com/callchain/orders.Repo")(0x...)
id = 7
(dlv) frame 1
> example.com/callchain/orders.(*Service).Fetch() ./orders/orders.go:47
(dlv) print id
7
(dlv) up
> example.com/callchain/orders.(*Handler).Describe() ./orders/orders.go:65
(dlv) print id
7
```

`stack` shows the chain innermost-first: `Find` was called by `Fetch`, which was
called by `Describe`, which was called by `main`. `frame 1` and `up` move the
cursor toward the caller so `print id` reads the value in each caller's scope,
letting you trace the id `7` down every layer without adding a single log line.
`down` moves back toward the innermost frame.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/callchain/orders"
)

func main() {
	repo := orders.NewRepo(orders.Order{ID: 7, Total: 100})
	h := orders.NewHandler(orders.NewService(repo))

	if line, err := h.Describe(7); err == nil {
		fmt.Println(line)
	}

	if _, err := h.Describe(99); errors.Is(err, orders.ErrNotFound) {
		fmt.Printf("id 99: %v\n", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
order 7 total 100
id 99: fetch order 99: order not found
```

### The test drives the chain and asserts the sentinel

Create `orders/orders_test.go`:

```go
package orders

import (
	"errors"
	"fmt"
	"testing"
)

func TestDescribe(t *testing.T) {
	t.Parallel()

	repo := NewRepo(Order{ID: 7, Total: 100}, Order{ID: 8, Total: 250})
	h := NewHandler(NewService(repo))

	tests := []struct {
		name    string
		id      int
		want    string
		wantErr error
	}{
		{name: "present", id: 7, want: "order 7 total 100"},
		{name: "other", id: 8, want: "order 8 total 250"},
		{name: "missing", id: 99, wantErr: ErrNotFound},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := h.Describe(tc.id)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Describe(%d) err = %v; want wrap of %v", tc.id, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Describe(%d) unexpected err: %v", tc.id, err)
			}
			if got != tc.want {
				t.Fatalf("Describe(%d) = %q; want %q", tc.id, got, tc.want)
			}
		})
	}
}

func ExampleHandler_Describe() {
	repo := NewRepo(Order{ID: 7, Total: 100})
	h := NewHandler(NewService(repo))
	line, _ := h.Describe(7)
	fmt.Println(line)
	// Output: order 7 total 100
}
```

### Scripted: capture the stack and a caller variable

```bash
go build -gcflags='all=-N -l' -o /tmp/callchain ./cmd/demo

cat > /tmp/callchain.dlv <<'EOF'
break example.com/callchain/orders.(*Repo).Find
continue
stack
frame 1
print id
quit
EOF

dlv exec /tmp/callchain --init /tmp/callchain.dlv 2>&1 | tee /tmp/callchain.out
grep -q 'id = 7' /tmp/callchain.out && echo OK
```

The captured stack lists `Find`, `Fetch`, `Describe`, `main` innermost-first, and
`print id` in frame 1 shows `7`: the id the service passed down, read from the
caller's frame without touching the code.

## Review

The chain is correct when `Describe` renders present orders and returns an error
wrapping `ErrNotFound` for missing ones, which `errors.Is` confirms in the test —
note the service wraps with `%w`, which is what makes `errors.Is` match through
the layers. The stack-walking proof is that `stack` prints the four frames
innermost-first and `frame 1`/`up` let `print id` read the same value at every
layer; if `print id` fails after `frame 1`, the cursor is on a frame where `id`
is not in scope, so check the frame you landed on. Use `args` for a frame's
parameters and `locals` for its declared variables; `up`/`down` are the
one-step cursor moves, `frame <n>` the absolute jump.

## Resources

- [Delve CLI command reference](https://github.com/go-delve/delve/blob/master/Documentation/cli/README.md) — `stack`/`bt`, `frame`, `up`, `down`, `args`, `locals`.
- [`errors` package](https://pkg.go.dev/errors) — `errors.Is` and wrapping with `%w`, the sentinel pattern the chain uses.
- [Go blog: working with errors](https://go.dev/blog/go1.13-errors) — why `fmt.Errorf("...: %w", err)` lets callers match a sentinel.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-scripted-ci-debugging.md](04-scripted-ci-debugging.md)
