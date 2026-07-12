# Exercise 6: gofmt as a Zero-Config CI Gate

`gofmt` is the canonical formatter: after it runs, every Go file in the ecosystem
is byte-identical in layout. This module builds a clean, formatted package, then
uses a deliberately messy file to walk `gofmt -l`, `-d`, `-s`, and `-w`, and wires
the one-line gate `test -z "$(gofmt -l .)"` that CI uses.

## What you'll build

```text
fmt-gate/                      module example.com/fmt-gate
  go.mod
  internal/
    slice/
      slice.go                 Tail, Head — already gofmt -s clean
      slice_test.go            table test
  cmd/
    demo/
      main.go                  prints Head/Tail of a slice
```

- Files: `internal/slice/slice.go`, `internal/slice/slice_test.go`, `cmd/demo/main.go`.
- Implement: `Tail` and `Head` slice helpers written in canonical, simplified form.
- Test: a table test of both helpers.
- Verify: `gofmt -l .` prints nothing (the gate passes); a messy file is shown named by `-l` and diffed by `-s -d`.

### The four modes and the one gate

`gofmt` has no configuration for *what* canonical looks like — that is the whole
value, because there is nothing to argue about in review. It has four operational
modes:

- `-l` lists the files that are not already canonical, one per line, and prints
  nothing when everything is clean. This is the read path CI uses.
- `-d` prints a unified diff of the change it would make, without touching the
  file. This is the eyeball path in review.
- `-w` writes the formatting in place.
- `-s` additionally applies safe *simplifications* — for example `s[1:len(s)]`
  becomes `s[1:]`, and a redundant element type inside a composite literal is
  dropped. It is off by default because simplification changes tokens, not just
  whitespace.

`go fmt ./...` is a thin wrapper that runs `gofmt -l -w` per package (note: it
does *not* pass `-s`). The CI gate is the composition
`test -z "$(gofmt -l .)"`: `gofmt -l .` emits the non-canonical filenames, the
`$(...)` captures them, and `test -z` succeeds only when that string is empty.
Any unformatted file makes the gate fail and names the file.

The package below is already canonical and already simplified (`s[1:]`, not
`s[1:len(s)]`), so `gofmt -l .` and `gofmt -s -l .` are both silent on it.

Create `internal/slice/slice.go`:

```go
package slice

// Tail returns all but the first element. A nil or empty slice yields nil.
func Tail(s []int) []int {
	if len(s) == 0 {
		return nil
	}
	return s[1:]
}

// Head returns the first element and true, or 0 and false for an empty slice.
func Head(s []int) (int, bool) {
	if len(s) == 0 {
		return 0, false
	}
	return s[0], true
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/fmt-gate/internal/slice"
)

func main() {
	nums := []int{10, 20, 30}
	h, _ := slice.Head(nums)
	fmt.Println("head:", h)
	fmt.Println("tail:", slice.Tail(nums))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
head: 10
tail: [20 30]
```

Create `internal/slice/slice_test.go`:

```go
package slice

import (
	"reflect"
	"testing"
)

func TestTail(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   []int
		want []int
	}{
		{"empty", nil, nil},
		{"one", []int{7}, []int{}},
		{"many", []int{1, 2, 3}, []int{2, 3}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Tail(tc.in)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Tail(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestHead(t *testing.T) {
	t.Parallel()
	if _, ok := Head(nil); ok {
		t.Fatal("Head(nil) ok = true, want false")
	}
	if got, ok := Head([]int{9, 8}); !ok || got != 9 {
		t.Fatalf("Head([9 8]) = %d,%v, want 9,true", got, ok)
	}
}
```

Confirm the module is already clean, then run the gate:

```bash
gofmt -l .
test -z "$(gofmt -l .)" && echo "gofmt gate: PASS"
```

```text
gofmt gate: PASS
```

`gofmt -l .` printed nothing, so the captured string is empty and `test -z`
succeeds.

### What a messy file looks like

Create a scratch file OUTSIDE this module to see the modes bite. It is
space-indented (a formatting defect) and uses `s[1:len(s)]` (a simplification
target):

```text
package messy

func Tail(s []int) []int {
    return s[1:len(s)]
}
```

`gofmt -l` names it:

```bash
gofmt -l messy.go
```

```text
messy.go
```

`gofmt -d` shows the whitespace fix (tabs, not spaces):

```text
@@ -1,5 +1,5 @@
 package messy

 func Tail(s []int) []int {
-    return s[1:len(s)]
+	return s[1:len(s)]
 }
```

`gofmt -s -d` shows the same whitespace fix plus the simplification
`s[1:len(s)]` becomes `s[1:]`:

```text
@@ -1,5 +1,5 @@
 package messy

 func Tail(s []int) []int {
-    return s[1:len(s)]
+	return s[1:]
 }
```

After `gofmt -s -w messy.go` (or `gofmt -s -w .` for the whole tree), the file is
canonical and simplified, and `test -z "$(gofmt -l .)"` passes again. This is
exactly why the module's own `Tail` was written as `s[1:]` from the start.

## Review

The module is correct when `test -z "$(gofmt -l .)"` succeeds and
`go test ./...` passes. The traps: hand-rolling a formatter that wraps `gofmt`
(the output drifts from the ecosystem and every review fills with whitespace
noise), and forgetting that `go fmt` does not apply `-s` — run `gofmt -s -w .` to
get simplifications. The gate is one line and requires no configuration; that is
the reason `gofmt` is a hard floor rather than a style preference.

## Resources

- [Command gofmt](https://pkg.go.dev/cmd/gofmt) — `-l`, `-d`, `-w`, `-s`, and what simplification does.
- [Effective Go — formatting](https://go.dev/doc/effective_go#formatting) — why the ecosystem standardized on `gofmt`.
- [go fmt](https://pkg.go.dev/cmd/go#hdr-Gofmt__reformat__package_sources) — the `go fmt` wrapper over `gofmt`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-go-doc-package-and-stdlib.md](05-go-doc-package-and-stdlib.md) | Next: [07-go-env-and-cross-compile-matrix.md](07-go-env-and-cross-compile-matrix.md)
