# Exercise 14: Paginated Lister With a Derived Cross-Field Limit

**Nivel: Intermedio** — validacion rapida (un test corto).

A paginated API client wrapper has three independent-looking knobs — page
size, a page count cap, and an optional target item count — but the target is
only reachable if the first two multiply out to at least that many items.
This module checks the one invariant that is a product of two other fields,
not a simple comparison between them.

## What you'll build

```text
lister/                    independent module: example.com/lister
  go.mod                   go 1.24
  lister.go                 Lister, Option, New, WithPageSize, WithMaxPages,
                            WithMaxTotalItems, WithStartCursor
  lister_test.go             table test over reachable and unreachable targets
```

- Implement `New(opts ...Option) (*Lister, error)` with four options, one of
  which sets a target a constructor-level check must prove is reachable.
- Test no target set, a reachable target, an unreachable target, an
  exact-boundary target, and each option's own input validation.

Set up the module:

```bash
mkdir -p go-solutions/04-functions/12-functional-options-pattern/14-paginated-lister-options
cd go-solutions/04-functions/12-functional-options-pattern/14-paginated-lister-options
go mod edit -go=1.24
```

Earlier modules check things like `queueDepth < workers` — a direct
comparison between two fields. This one is different: `pageSize * maxPages`
is a *derived* value (the most items the lister could ever fetch) compared
against a third field. No single option can compute that product, because
each only knows its own argument; only `New`, after every option has run,
has all three values in hand. `WithMaxTotalItems` defaults to zero, meaning
"no target," which skips the check entirely for a caller who never sets it.

Create `lister.go`:

```go
package lister

import "fmt"

type Lister struct {
	pageSize      int
	maxPages      int
	maxTotalItems int
	startCursor   string
}

type Option func(*Lister) error

// New seeds defaults, applies opts in order, then checks the derived
// invariant no single option could see: pageSize * maxPages must be able to
// reach maxTotalItems.
func New(opts ...Option) (*Lister, error) {
	l := &Lister{pageSize: 50, maxPages: 20}
	for _, opt := range opts {
		if err := opt(l); err != nil {
			return nil, err
		}
	}
	if l.maxTotalItems > 0 && l.pageSize*l.maxPages < l.maxTotalItems {
		return nil, fmt.Errorf("pageSize %d * maxPages %d = %d, which cannot reach maxTotalItems %d",
			l.pageSize, l.maxPages, l.pageSize*l.maxPages, l.maxTotalItems)
	}
	return l, nil
}

func WithPageSize(n int) Option {
	return func(l *Lister) error {
		if n < 1 || n > 1000 {
			return fmt.Errorf("page size must be between 1 and 1000, got %d", n)
		}
		l.pageSize = n
		return nil
	}
}

func WithMaxPages(n int) Option {
	return func(l *Lister) error {
		if n < 1 {
			return fmt.Errorf("max pages must be >= 1, got %d", n)
		}
		l.maxPages = n
		return nil
	}
}

// WithMaxTotalItems sets the target item count (>= 1). Zero, the default,
// means no target and disables the cross check.
func WithMaxTotalItems(n int) Option {
	return func(l *Lister) error {
		if n < 1 {
			return fmt.Errorf("max total items must be >= 1, got %d", n)
		}
		l.maxTotalItems = n
		return nil
	}
}

func WithStartCursor(cursor string) Option {
	return func(l *Lister) error {
		if cursor == "" {
			return fmt.Errorf("start cursor must not be empty")
		}
		l.startCursor = cursor
		return nil
	}
}

func (l *Lister) PageSize() int       { return l.pageSize }
func (l *Lister) MaxPages() int       { return l.maxPages }
func (l *Lister) MaxTotalItems() int  { return l.maxTotalItems }
func (l *Lister) StartCursor() string { return l.startCursor }
```

Create `lister_test.go`:

```go
package lister

import "testing"

func TestNew(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		opts    []Option
		wantErr bool
	}{
		{name: "defaults only, no target set"},
		{name: "reachable target", opts: []Option{WithPageSize(50), WithMaxPages(20), WithMaxTotalItems(500)}},
		{name: "unreachable target", opts: []Option{WithPageSize(10), WithMaxPages(2), WithMaxTotalItems(100)}, wantErr: true},
		{name: "invalid page size", opts: []Option{WithPageSize(0)}, wantErr: true},
		{name: "page size over the cap", opts: []Option{WithPageSize(5000)}, wantErr: true},
		{name: "empty start cursor", opts: []Option{WithStartCursor("")}, wantErr: true},
		{name: "exact boundary is reachable", opts: []Option{WithPageSize(10), WithMaxPages(10), WithMaxTotalItems(100)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			l, err := New(tt.opts...)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if l.PageSize() < 1 || l.MaxPages() < 1 {
				t.Fatalf("invalid built lister: %+v", l)
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

The lister is correct when a configured target is always mathematically
reachable given the page size and page cap, and when the absence of a target
never triggers a check that does not apply. The exact-boundary case matters
as much as the unreachable one: a strict `<` rather than `<=` would either
reject a target that exactly matches capacity or accept one item short of it,
and only a boundary test catches which mistake was made. Not every
cross-field invariant is a direct comparison — some are a computed
relationship among three or more fields, and that computation still belongs
in the constructor, the one place every field is visible at once.

## Resources

- [Stripe API: pagination](https://docs.stripe.com/api/pagination)
- [Google AIP-158: Pagination](https://google.aip.dev/158)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [13-mutually-exclusive-auth-options.md](13-mutually-exclusive-auth-options.md) | Next: [15-distributed-trace-span-options.md](15-distributed-trace-span-options.md)
