# Exercise 2: Isolate One Iteration with Conditional Breakpoints

When a loop runs over ten thousand records and exactly one triggers a bug,
pressing `continue` until you reach it is not debugging, it is data entry. A
conditional breakpoint attaches a Go expression to a breakpoint so Delve stops
only on the iteration you care about. This module builds a batch validator with
one bad record buried in a large batch and isolates it in a single `continue`.

This module is fully self-contained: its own `go mod init`, demo, and test.

## What you'll build

```text
batchval/                  independent module: example.com/batchval
  go.mod                   go 1.24
  batch/
    batch.go               type Record; FirstInvalid([]Record) (int, bool)
  cmd/
    demo/
      main.go              builds a batch, prints the offending index
  batch/batch_test.go      table-driven TestFirstInvalid + Example
```

- Files: `batch/batch.go`, `cmd/demo/main.go`, `batch/batch_test.go`.
- Implement: `FirstInvalid(records []Record) (int, bool)` returning the index of the first record with a negative `Amount`.
- Test: deterministic batch with exactly one invalid record; assert its index.
- Verify: `go test -count=1 -race ./...`, then a scripted `dlv` session whose condition lands once on the bad record.

Set up the module:

```bash
go mod edit -go=1.24
```

### Why a condition beats N continues

A breakpoint on the loop body fires on every iteration. In a batch of a thousand
records that is a thousand stops, and finding the one with `Amount < 0` by eye is
hopeless. `condition <bpid> <expr>` tells Delve to evaluate `<expr>` in the
breakpoint's scope on every hit and stop only when it is true. The cost is one
expression evaluation per iteration — negligible next to the alternative of a
human pressing `continue` a thousand times. The expression must reference
variables in scope where the breakpoint sits: inside the range loop the loop
variable `r` is in scope, so `r.Amount < 0` is valid.

The companion command is `on <bpid> print <expr>`, which prints an expression
every time the breakpoint fires without stopping. Combined with a condition, `on`
lets you log just the offending iterations and keep running, which is how you turn
a breakpoint into a targeted trace.

Create `batch/batch.go`:

```go
package batch

// Record is one line item in a batch. A negative Amount is invalid.
type Record struct {
	ID     int
	Amount int
}

// FirstInvalid returns the index of the first record whose Amount is negative,
// and whether such a record exists. It is a plain linear scan; the point of the
// exercise is to stop the scan on exactly that record under Delve.
func FirstInvalid(records []Record) (int, bool) {
	for i, r := range records {
		if r.Amount < 0 {
			return i, true
		}
	}
	return -1, false
}
```

### Isolating the bad record in one continue

Build a batch where exactly one record is negative and debug it. Set a breakpoint
on the comparison line, attach the condition, and continue once:

```bash
dlv debug ./cmd/demo
```

```text
(dlv) break batch/batch.go:14
Breakpoint 1 set at 0x... for example.com/batchval/batch.FirstInvalid() ./batch/batch.go:14
(dlv) condition 1 r.Amount < 0
(dlv) continue
> example.com/batchval/batch.FirstInvalid() ./batch/batch.go:14 (hits goroutine(1):1 total:1)
(dlv) print i
617
(dlv) print r
example.com/batchval/batch.Record {ID: 6170, Amount: -5}
```

Delve skipped every record with a non-negative amount and stopped on index 617,
the single bad one, on the first `continue`. `cond` is an accepted alias for
`condition`, so `cond 1 r.Amount < 0` is identical. To print instead of stop, set
`on 1 print r` and the offending record prints as the scan runs past it.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/batchval/batch"
)

func main() {
	records := make([]batch.Record, 1000)
	for i := range records {
		records[i] = batch.Record{ID: i * 10, Amount: i + 1}
	}
	records[617].Amount = -5 // the single bad record

	if idx, ok := batch.FirstInvalid(records); ok {
		fmt.Printf("first invalid at index %d: %+v\n", idx, records[idx])
	} else {
		fmt.Println("all records valid")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
first invalid at index 617: {ID:6170 Amount:-5}
```

### The test pins the offending index

Create `batch/batch_test.go`:

```go
package batch

import (
	"fmt"
	"testing"
)

func TestFirstInvalid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		in       []Record
		wantIdx  int
		wantBool bool
	}{
		{name: "none", in: []Record{{ID: 1, Amount: 5}, {ID: 2, Amount: 3}}, wantIdx: -1, wantBool: false},
		{name: "first", in: []Record{{ID: 1, Amount: -1}, {ID: 2, Amount: 3}}, wantIdx: 0, wantBool: true},
		{name: "middle", in: []Record{{ID: 1, Amount: 5}, {ID: 2, Amount: -2}, {ID: 3, Amount: 7}}, wantIdx: 1, wantBool: true},
		{name: "empty", in: nil, wantIdx: -1, wantBool: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			idx, ok := FirstInvalid(tc.in)
			if idx != tc.wantIdx || ok != tc.wantBool {
				t.Fatalf("FirstInvalid(%v) = %d,%v; want %d,%v", tc.in, idx, ok, tc.wantIdx, tc.wantBool)
			}
		})
	}
}

func ExampleFirstInvalid() {
	recs := []Record{{ID: 1, Amount: 5}, {ID: 2, Amount: -2}, {ID: 3, Amount: 7}}
	idx, ok := FirstInvalid(recs)
	fmt.Println(idx, ok)
	// Output: 1 true
}
```

### Scripted: prove the condition fired once

```bash
go build -gcflags='all=-N -l' -o /tmp/batchval ./cmd/demo

cat > /tmp/batchval.dlv <<'EOF'
break batch/batch.go:14
condition 1 r.Amount < 0
continue
print i
print r.Amount
quit
EOF

dlv exec /tmp/batchval --init /tmp/batchval.dlv 2>&1 | tee /tmp/batchval.out
grep -q 'i = 617' /tmp/batchval.out && echo OK
```

The captured output shows `i = 617` and `r.Amount = -5`: the condition made Delve
land directly on the one offending iteration, which a CI job can assert with the
`grep`.

## Review

The validator is correct when it returns the first index whose `Amount` is
negative and reports absence with `(-1, false)`, which the table pins across the
none/first/middle/empty cases. The conditional-breakpoint proof is that a single
`continue` lands on index 617 rather than iterating a thousand times — if it
stops earlier, your condition references the wrong variable or the wrong scope.
Remember the condition expression is evaluated in the breakpoint's scope: `r` is
valid on the comparison line because the range loop binds it there. Use `on <bpid>
print` when you want to observe every matching iteration without halting, and
`cond` as the shorthand for `condition`.

## Resources

- [Delve CLI command reference](https://github.com/go-delve/delve/blob/master/Documentation/cli/README.md) — `condition`, `cond`, and `on` documented with their scopes.
- [`dlv debug` usage](https://github.com/go-delve/delve/blob/master/Documentation/usage/dlv_debug.md) — launching a build under the debugger.
- [Go range statement](https://go.dev/ref/spec#For_range) — what `i, r := range records` binds in the loop scope Delve evaluates the condition in.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-stack-and-frames.md](03-stack-and-frames.md)
