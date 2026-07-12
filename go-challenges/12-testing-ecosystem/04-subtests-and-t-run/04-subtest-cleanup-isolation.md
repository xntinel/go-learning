# Exercise 4: Per-Subtest Fixtures with t.Cleanup and t.TempDir

When a batch of handler cases each needs its own scratch directory, its own env
override, and its own response recorder, the isolation has to be per subtest — one
case must not see another's files or leftover env. This exercise tests a
file-serving HTTP handler across subtests, giving each case a fresh `t.TempDir`, a
scoped `t.Setenv`, and cleanups that run in LIFO order after the subtest finishes.

This module is fully self-contained: its own `go mod init`, handler, demo, and
tests. Nothing here imports any other exercise.

## What you'll build

```text
fileapi/                    independent module: example.com/fileapi
  go.mod                    go 1.26
  handler.go                func Handler(w, r) serving files from $DATA_DIR
  cmd/
    demo/
      main.go               runnable demo: serve a temp file, then a missing one
  handler_test.go           per-subtest TempDir/Setenv; LIFO cleanup order; isolation
```

- Files: `handler.go`, `cmd/demo/main.go`, `handler_test.go`.
- Implement: `Handler(w http.ResponseWriter, r *http.Request)` serving the file
  named by the request path from the directory in `$DATA_DIR`.
- Test: per-subtest `t.TempDir` and `t.Setenv`; assert cleanups run LIFO; assert
  each subtest gets a distinct temp dir; assert `DATA_DIR` is restored afterward.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/04-subtests-and-t-run/04-subtest-cleanup-isolation/cmd/demo
cd go-solutions/12-testing-ecosystem/04-subtests-and-t-run/04-subtest-cleanup-isolation
```

### Why these fixtures, and why not parallel

`t.TempDir()` returns a fresh directory unique to the calling test or subtest,
auto-removed when it and its subtests finish — so calling it inside each `t.Run`
gives every case an isolated working directory with zero manual `os.RemoveAll`.
`t.Setenv` sets an env var and restores the previous value on cleanup, which lets
each case point `$DATA_DIR` at its own temp dir and be sure the value is gone
afterward. `t.Cleanup(f)` registers teardown that runs when the subtest finishes,
last-registered-first — the ordering you want when a later resource depends on an
earlier one.

The hard constraint that shapes this whole file: `t.Setenv` *forbids* parallel
tests. An env var is process-global; mutating it while sibling tests run
concurrently is a data race across the process, so `t.Setenv` panics if the test
or any ancestor is parallel. Therefore this module uses **no** `t.Parallel` — the
cases run serially, which is also what makes the LIFO-order and distinct-dir
assertions deterministic. If you needed these cases to run in parallel, you would
have to stop using the environment and pass the directory in explicitly.

Create `handler.go`:

```go
package fileapi

import (
	"net/http"
	"os"
	"path/filepath"
)

// Handler serves the contents of the file named by the last path segment of the
// request, read from the directory named by the DATA_DIR environment variable.
// A missing file yields 404; a request with no file name yields 400.
func Handler(w http.ResponseWriter, r *http.Request) {
	name := filepath.Base(r.URL.Path)
	if name == "." || name == "/" || name == "" {
		http.Error(w, "no file specified", http.StatusBadRequest)
		return
	}
	data, err := os.ReadFile(filepath.Join(os.Getenv("DATA_DIR"), name))
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(data)
}
```

### The runnable demo

The demo wires the handler to an `httptest.NewRecorder` (the same in-memory
`http.ResponseWriter` the tests use) so it runs with no network. It seeds a temp
dir, points `DATA_DIR` at it, and serves one present and one missing file.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"

	"example.com/fileapi"
)

func main() {
	dir, err := os.MkdirTemp("", "fileapi-demo")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hello from disk"), 0o644); err != nil {
		panic(err)
	}
	os.Setenv("DATA_DIR", dir)

	rec := httptest.NewRecorder()
	fileapi.Handler(rec, httptest.NewRequest(http.MethodGet, "/notes.txt", nil))
	fmt.Printf("GET /notes.txt   -> %d %q\n", rec.Code, rec.Body.String())

	rec = httptest.NewRecorder()
	fileapi.Handler(rec, httptest.NewRequest(http.MethodGet, "/missing.txt", nil))
	fmt.Printf("GET /missing.txt -> %d\n", rec.Code)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
GET /notes.txt   -> 200 "hello from disk"
GET /missing.txt -> 404
```

### Tests

`TestHandler` gives each case its own temp dir and scoped `DATA_DIR`, then drives
the handler through an in-memory recorder. `TestCleanupLIFO` registers three
cleanups in a child subtest and, after the child returns, asserts they ran in
reverse order. `TestTempDirIsolation` records the temp dir each child got and
asserts they differ. `TestSetenvScoped` confirms `DATA_DIR` is restored to empty
once the subtest that set it has finished.

Create `handler_test.go`:

```go
package fileapi

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestHandler(t *testing.T) {
	cases := []struct {
		name       string
		writeFile  bool
		target     string
		wantStatus int
		wantBody   string
	}{
		{"served", true, "/notes.txt", http.StatusOK, "hello from disk"},
		{"missing", false, "/notes.txt", http.StatusNotFound, ""},
		{"no_name", false, "/", http.StatusBadRequest, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("DATA_DIR", dir)
			if tc.writeFile {
				if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hello from disk"), 0o644); err != nil {
					t.Fatalf("seed file: %v", err)
				}
			}

			rec := httptest.NewRecorder()
			Handler(rec, httptest.NewRequest(http.MethodGet, tc.target, nil))

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if tc.wantBody != "" && rec.Body.String() != tc.wantBody {
				t.Fatalf("body = %q, want %q", rec.Body.String(), tc.wantBody)
			}
		})
	}
}

func TestCleanupLIFO(t *testing.T) {
	var order []int
	t.Run("child", func(t *testing.T) {
		for i := range 3 {
			id := i
			t.Cleanup(func() { order = append(order, id) })
		}
	})
	// t.Run returns only after the child's cleanups have run, LIFO.
	want := []int{2, 1, 0}
	if len(order) != len(want) {
		t.Fatalf("cleanup order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("cleanup order = %v, want %v", order, want)
		}
	}
}

func TestTempDirIsolation(t *testing.T) {
	var dirs []string
	for _, name := range []string{"first", "second"} {
		t.Run(name, func(t *testing.T) {
			dirs = append(dirs, t.TempDir())
		})
	}
	if dirs[0] == dirs[1] {
		t.Fatalf("both subtests got the same temp dir %q", dirs[0])
	}
}

func TestSetenvScoped(t *testing.T) {
	t.Run("sets_it", func(t *testing.T) {
		t.Setenv("DATA_DIR", "/scoped/value")
		if got := os.Getenv("DATA_DIR"); got != "/scoped/value" {
			t.Fatalf("inside subtest DATA_DIR = %q, want /scoped/value", got)
		}
	})
	if got := os.Getenv("DATA_DIR"); got != "" {
		t.Fatalf("after subtest DATA_DIR = %q, want restored to empty", got)
	}
}
```

## Review

Isolation here is a property of where you put the fixtures: `t.TempDir` and
`t.Setenv` *inside* each `t.Run` scope the directory and the env override to that
one case, and their auto-cleanup means no case leaks state into the next. The LIFO
test makes the cleanup-ordering contract concrete — registered `0,1,2`, they run
`2,1,0`, which is why you register in dependency (acquire) order and let LIFO tear
down in reverse. The load-bearing constraint is that none of this can be parallel:
`t.Setenv` panics under a parallel ancestor because it mutates process-global
state, so if you need parallel cases you must pass configuration explicitly rather
than through the environment.

## Resources

- [testing.T.Cleanup — pkg.go.dev](https://pkg.go.dev/testing#T.Cleanup)
- [testing.T.TempDir — pkg.go.dev](https://pkg.go.dev/testing#T.TempDir)
- [testing.T.Setenv — pkg.go.dev](https://pkg.go.dev/testing#T.Setenv)
- [net/http/httptest — pkg.go.dev](https://pkg.go.dev/net/http/httptest)

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-parallel-subtests-lifecycle.md](05-parallel-subtests-lifecycle.md)
