# Exercise 5: Per-tenant subtree isolation with fs.Sub and path-escape rejection

Multi-tenant services must guarantee that tenant A can never read tenant B's
files. `fs.Sub` scopes an `fs.FS` to a subtree, and the rooted `fs.FS` contract
makes that scope a real boundary: `..` never validates, so a crafted traversal
path is rejected before it reaches the filesystem. This exercise builds a
`TenantFS` scoper plus a guarded read and proves both the isolation and the
traversal rejection with `MapFS`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
tenantfs/                    independent module: example.com/tenantfs
  go.mod                     go 1.26
  tenantfs.go                TenantFS(root, id) (fs.FS, error); Read(fs.FS, name) with guard
  cmd/
    demo/
      main.go                scope to tenant a, read a's file, fail to read b's
  tenantfs_test.go           isolation, cross-tenant not-exist, traversal-rejected tests
```

- Files: `tenantfs.go`, `cmd/demo/main.go`, `tenantfs_test.go`.
- Implement: `TenantFS(root fs.FS, tenantID string) (fs.FS, error)` returning an
  `fs.Sub`-scoped view; `Read(fsys fs.FS, name string) ([]byte, error)` that
  rejects absolute paths and `..` traversal via `fs.ValidPath` before touching
  the FS.
- Test: `MapFS` holding `tenants/a/...` and `tenants/b/...`; after scoping to
  tenant a, a's files read and any reference to b resolves to `fs.ErrNotExist`;
  a `../b/secret` input is rejected with `fs.ErrInvalid`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/11-testing-filesystems-with-fstest/05-fs-sub-tenant-scoping/cmd/demo
cd go-solutions/12-testing-ecosystem/11-testing-filesystems-with-fstest/05-fs-sub-tenant-scoping
```

### Two layers of defense, both from the fs.FS contract

The isolation rests on two independent mechanisms, and it is worth seeing that
they are distinct.

The first is `fs.Sub(root, "tenants/"+tenantID)`. It returns an `fs.FS` in which
every `Open("x")` resolves to `tenants/<id>/x` on the parent. Tenant code holding
this sub-FS sees only its own subtree — tenant B's files are not merely
forbidden, they are *not present*, so a request for them returns
`fs.ErrNotExist`. There is no code path from the sub-FS to a sibling tenant's
directory, because the sub-FS has no notion of a parent to climb to.

The second is an explicit `fs.ValidPath` guard on the incoming name, *before*
dispatch. Even within a tenant's subtree, a caller might pass hostile input:
`../b/secret`, `/etc/passwd`, `./x/../../y`. Every one of those violates
`fs.ValidPath` — a leading slash, a `..` element, an un-cleaned path — so the
guard rejects it with `fs.ErrInvalid` and the FS is never touched. This matters
because it stops the attack at the door rather than relying on `Sub` to have
cleaned it: defense in depth. The guard is one `if !fs.ValidPath(name)` and it
is the difference between a scoper you can trust with user-supplied filenames and
one you cannot.

Note the belt-and-suspenders reality: even if the guard were absent, `fs.Sub`'s
underlying `Open` also validates, so `..` still could not escape. The explicit
guard exists so *your* API returns a clean `fs.ErrInvalid` you control, at the
boundary, rather than deferring to whatever the inner FS happens to do.

Create `tenantfs.go`:

```go
package tenantfs

import (
	"fmt"
	"io/fs"
)

// TenantFS returns a view of root scoped to a single tenant's subtree under
// tenants/<tenantID>. Files outside that subtree are not reachable through the
// returned fs.FS.
func TenantFS(root fs.FS, tenantID string) (fs.FS, error) {
	dir := "tenants/" + tenantID
	if !fs.ValidPath(dir) {
		return nil, fmt.Errorf("tenantfs: invalid tenant id %q", tenantID)
	}
	return fs.Sub(root, dir)
}

// Read reads a file from fsys after validating name against the fs.FS contract.
// Absolute paths and dot-dot traversal are rejected before the FS is touched.
func Read(fsys fs.FS, name string) ([]byte, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "read", Path: name, Err: fs.ErrInvalid}
	}
	return fs.ReadFile(fsys, name)
}
```

### The runnable demo

The demo builds a root holding two tenants, scopes to tenant a, reads a's secret
through the scoped view, and then shows that the same relative name that reaches
b in the root does not resolve inside a's view.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"io/fs"
	"testing/fstest"

	"example.com/tenantfs"
)

func main() {
	root := fstest.MapFS{
		"tenants/a/secret.txt": {Data: []byte("alice-token")},
		"tenants/b/secret.txt": {Data: []byte("bob-token")},
	}

	aFS, err := tenantfs.TenantFS(root, "a")
	if err != nil {
		fmt.Println("scope error:", err)
		return
	}

	data, _ := tenantfs.Read(aFS, "secret.txt")
	fmt.Printf("tenant a reads: %s\n", data)

	_, err = tenantfs.Read(aFS, "../b/secret.txt")
	fmt.Printf("traversal rejected: %v\n", errors.Is(err, fs.ErrInvalid))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
tenant a reads: alice-token
traversal rejected: true
```

### Tests

`TestIsolation` scopes to tenant a and asserts a's file is readable.
`TestCrossTenantNotExist` asserts that a valid-but-foreign name (`b/secret.txt`)
resolves to `fs.ErrNotExist` inside a's view — proving the sibling tenant is
simply absent, not merely forbidden. `TestTraversalRejected` asserts that
`../b/secret.txt` is rejected with `fs.ErrInvalid` by the guard before any
dispatch. `TestInvalidTenantID` asserts a bad tenant id is rejected at scope
time.

Create `tenantfs_test.go`:

```go
package tenantfs

import (
	"errors"
	"io/fs"
	"testing"
	"testing/fstest"
)

func rootFS() fstest.MapFS {
	return fstest.MapFS{
		"tenants/a/secret.txt": {Data: []byte("alice-token")},
		"tenants/a/config.yml": {Data: []byte("region: us")},
		"tenants/b/secret.txt": {Data: []byte("bob-token")},
	}
}

func TestIsolation(t *testing.T) {
	t.Parallel()

	aFS, err := TenantFS(rootFS(), "a")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Read(aFS, "secret.txt")
	if err != nil {
		t.Fatalf("Read a's secret: %v", err)
	}
	if string(got) != "alice-token" {
		t.Fatalf("got %q, want alice-token", got)
	}
}

func TestCrossTenantNotExist(t *testing.T) {
	t.Parallel()

	aFS, err := TenantFS(rootFS(), "a")
	if err != nil {
		t.Fatal(err)
	}
	// A valid path that names another tenant is simply not present in a's view.
	_, err = Read(aFS, "b/secret.txt")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want errors.Is fs.ErrNotExist", err)
	}
}

func TestTraversalRejected(t *testing.T) {
	t.Parallel()

	aFS, err := TenantFS(rootFS(), "a")
	if err != nil {
		t.Fatal(err)
	}
	_, err = Read(aFS, "../b/secret.txt")
	if !errors.Is(err, fs.ErrInvalid) {
		t.Fatalf("err = %v, want errors.Is fs.ErrInvalid", err)
	}
}

func TestInvalidTenantID(t *testing.T) {
	t.Parallel()

	if _, err := TenantFS(rootFS(), "../b"); err == nil {
		t.Fatal("TenantFS accepted a traversal tenant id")
	}
}
```

## Review

Isolation is correct when tenant a's view can read a's files, resolves any
foreign-but-valid name to `fs.ErrNotExist`, and rejects any traversal name with
`fs.ErrInvalid` before dispatch. The two mechanisms are independent and both
matter: `fs.Sub` makes sibling tenants absent, and the `fs.ValidPath` guard
turns hostile input into a controlled `fs.ErrInvalid` at your API boundary. The
mistake to avoid is passing user-supplied filenames straight to `fs.ReadFile`
without the guard — even though `Sub` would also block escape, you want the clean
sentinel and the rejection at the door, not buried in the inner FS.

## Resources

- [`fs.Sub`](https://pkg.go.dev/io/fs#Sub) — subtree scoping, the isolation primitive.
- [`fs.ValidPath`](https://pkg.go.dev/io/fs#ValidPath) — why `..` and leading slashes never validate.
- [`fs.ReadFile` and `fs.ErrNotExist`](https://pkg.go.dev/io/fs#ReadFile) — the read helper and the not-present sentinel.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-asset-server-fileserverfs.md](04-asset-server-fileserverfs.md) | Next: [06-glob-config-discovery.md](06-glob-config-discovery.md)
