# Testing Filesystems with testing/fstest and the fs.FS Abstraction — Concepts

Almost every backend service touches a filesystem: a config loader reads
`config.yaml`, a migration runner scans a `migrations/` directory, a template
engine loads `*.tmpl`, an asset handler serves `/static/`. The naive way to
test any of these is to build a `t.TempDir`, `os.WriteFile` some fixtures, run
the code, and hope the cleanup fires. That path is slow (real disk I/O),
non-deterministic (directory read order is OS-dependent), race-prone under
`-race` (temp-dir cleanup interleaving), and it couples your test to the host
filesystem's quirks. The load-bearing skill this lesson teaches is not an API —
it is an architectural boundary. Senior backend code should depend on the
`fs.FS` interface, never on `os` directly. Once your config loader, migration
runner, and asset server take an `fs.FS`, production supplies `os.DirFS` or an
`embed.FS`, and tests supply an in-memory `fstest.MapFS` with zero disk I/O and
zero cleanup. Read this file once and you have the model behind all nine
independent exercises that follow.

## Concepts

### The fs.FS contract: rooted, slash-separated, no dot-dot

`fs.FS` is a one-method interface: `Open(name string) (fs.File, error)`. Its
power is not the method — it is the *contract on `name`*, encoded in
`fs.ValidPath`. A valid `fs.FS` path is a rooted, unrooted-looking,
slash-separated, cleaned sequence of elements: `x/y/z`. No leading slash
(`/etc/passwd` is invalid), no trailing slash, no `.` or `..` element, no empty
element, no backslashes as separators. The single special case is that the root
directory is named `.`. This is deliberate: because `..` never validates, a
path passed through an `fs.FS` can never escape the root. That property is what
makes `fs.Sub` a real security boundary for per-tenant isolation, and it is why
depending on `fs.FS` instead of raw `os.Open(userInput)` closes an entire class
of path-traversal bugs. Production supplies the implementation — `os.DirFS("/srv/app")`
roots the OS filesystem at a directory, `embed.FS` bakes files into the binary —
and tests supply `fstest.MapFS`. The code under test never knows the difference.

### fstest.MapFS: an in-memory filesystem with no disk and no cleanup

`fstest.MapFS` is `map[string]*fstest.MapFile`. Each key is a valid `fs.FS`
path (no leading slash), each value carries the file's `Data`, `Mode`,
`ModTime`, and `Sys`. It is a full `fs.FS` implemented over a Go map: zero disk
I/O, deterministic `ReadDir` ordering (sorted, always), no `t.TempDir`, nothing
to clean up, and it is safe to build fresh inside every parallel subtest.
Crucially, `MapFS` implements the optional fast-path interfaces a real
filesystem would — `ReadDirFS`, `ReadFileFS`, `GlobFS`, `SubFS`, `StatFS` (and,
since Go 1.25, `ReadLinkFS`/`LstatFS`) — so code that dispatches to those
interfaces takes the same fast path in a test that it would in production. A
`MapFS` fixture is not a toy stand-in; it exercises the same code paths as
`os.DirFS`.

### MapFile drives the time and permission dimensions deterministically

`MapFile` carries more than bytes. `Mode` (an `fs.FileMode`) lets a fixture
present a directory or a specific permission set; `ModTime` (a `time.Time`)
lets a test drive time-sensitive logic — a hot-reload cache that re-parses only
when the file is newer, a conditional HTTP GET that returns `304 Not Modified` —
with a fixed, chosen instant and no real clock. Swapping a `MapFS` entry for one
with a later `ModTime` simulates an on-disk edit with no `time.Sleep` and no
wall-clock dependence. This is the same discipline synctest brings to
concurrency: make the nondeterministic dimension (here, time and file state) an
explicit, injected value instead of a real-world side effect.

### fstest.TestFS validates any custom fs.FS against the contract

The day you write your own `fs.FS` — a prefix adapter, a union filesystem, a
read-through cache — you inherit the whole contract, and it is easy to get
subtly wrong: `ReadDir` returning unsorted entries, `Stat` disagreeing with
`Open`, an invalid path that should be rejected slipping through, `Seek`/`ReadAt`
inconsistency. `fstest.TestFS(fsys, expected...)` walks the entire tree, opens
and re-reads every file, and checks all of that. It also asserts the tree
contains at least the `expected` paths (or, with no expected paths, that the FS
is empty). Run it against any custom implementation before you trust it —
`if err := fstest.TestFS(myFS, "a/b.txt"); err != nil { t.Fatal(err) }`. It is
the filesystem equivalent of a contract test.

### Top-level helpers dispatch to optional interfaces

`io/fs` ships free functions that operate on any `fs.FS`: `fs.ReadFile`,
`fs.ReadDir`, `fs.Glob`, `fs.Sub`, `fs.Stat`, `fs.WalkDir`. Each one checks
whether the concrete FS implements a matching optional interface and takes the
fast path if so, else falls back to the primitive `Open`. `fs.ReadFile(fsys, n)`
calls `fsys.ReadFile(n)` if `fsys` is a `ReadFileFS`, otherwise it does
`Open` + `io.ReadAll` + `Close`. `fs.ReadDir` calls `ReadDirFS.ReadDir`
otherwise `Open` + `ReadDirFile.ReadDir(-1)`. `fs.Glob` calls `GlobFS.Glob`
otherwise it walks with `ReadDir`. `fs.Sub` calls `SubFS.Sub` otherwise wraps.
The practical consequence: if you write a custom `fs.FS` for a hot config path
and implement only `Open`, every `fs.ReadFile` pays an extra `Open`/`Stat`/read
cycle. Implementing `ReadFileFS` collapses that to a single method call. You can
*prove* which path is taken by instrumenting your wrapper with atomic counters.

### fs errors are sentinel-wrapped; test with errors.Is

A missing file yields a `*fs.PathError` whose `Err` field is `fs.ErrNotExist`;
an invalid path yields `fs.ErrInvalid`; a permission failure yields
`fs.ErrPermission`. Because `*fs.PathError` unwraps to that sentinel,
`errors.Is(err, fs.ErrNotExist)` is the correct, portable check. Never
`strings.Contains(err.Error(), "no such file")` — the message is OS-dependent
and unstructured. `os.IsNotExist(err)` still works for legacy reasons, but
`errors.Is(err, fs.ErrNotExist)` is the modern form and composes with your own
`%w`-wrapped errors.

### MapFS's deliberate blind spot: it never fails a read

Every file stored in a `MapFS` reads cleanly to EOF. `MapFS` cannot simulate a
mid-stream read error, a permission denial on `Open`, or a truncated read — the
exact failure modes your error-handling branches exist to cover. This is not a
bug in `MapFS`; a map-backed FS has no I/O to fail. It means `MapFS` alone
leaves your error paths uncovered. To exercise them you hand-write a small
fault-injecting `fs.FS` wrapper (and a faulty `fs.File`) that returns a
configured error from `Open` or from `Read` for a named path, composed over a
normal `MapFS` so most files behave and one misbehaves. That wrapper is how you
prove a config loader surfaces an I/O error instead of silently returning a
zero-valued `Config`.

### fs.WalkDir walks lexically over cheap DirEntry values

`fs.WalkDir(fsys, root, fn)` walks the tree in lexical order, calling `fn` with
a `fs.DirEntry` (not a `fs.FileInfo`) for each node. `DirEntry` is cheaper — it
carries name and type without a per-entry `Stat`, so a walk over thousands of
files does not pay a `Stat` per file. When a directory cannot be read, `WalkDir`
passes the error into `fn` as its third argument; a callback that ignores that
argument silently produces an incomplete walk. Return `fs.SkipDir` from `fn` to
prune the current directory, `fs.SkipAll` to stop the whole walk, or any other
error to abort and propagate. This is the backbone of migration discovery and
any "find all files matching a shape" scan.

### fs.Sub scopes a subtree; the rooted contract blocks traversal

`fs.Sub(fsys, dir)` returns an `fs.FS` rooted at `dir` — every `Open("x")` on
the sub-FS resolves to `dir/x` on the parent. It is the primary mechanism for
multi-tenant or per-root isolation: `sub, _ := fs.Sub(root, "tenants/"+id)`
hands tenant code a view in which its own files are visible and every other
tenant's are simply not present. Combined with `fs.ValidPath`, it blocks
traversal by construction: a crafted `../other/secret` never validates, so it is
rejected before any dispatch, and even a name that reached the FS could not
climb above `dir` because `..` is not a legal element.

### net/http serves an fs.FS directly

`http.FileServerFS(fsys)`, `http.ServeFileFS(w, r, fsys, name)`, and `http.FS`
all take an `fs.FS`, so a static-asset handler serves an `embed.FS` in
production and a `fstest.MapFS` in tests with byte-identical handler code — and
you exercise it through `httptest.NewRecorder` with no listen socket. Content-Type
sniffing, `Range` requests, and `If-Modified-Since`/`304` conditional handling
all come from `net/http` for free, keyed off the `ModTime` that your `MapFile`
supplies. (The files served must implement `io.Seeker`; `MapFS` files do.)

## Common Mistakes

### Calling os.ReadFile/os.Open inside injectable logic

Wrong: a `Load(path string)` that calls `os.ReadFile(path)` internally. The only
way to test it is a real temp file. Fix: `Load(fsys fs.FS, name string)` using
`fs.ReadFile`; tests inject a `MapFS`, production injects `os.DirFS` or
`embed.FS`. The signature change is the whole fix.

### Faking a filesystem with t.TempDir + os.WriteFile

Wrong: `dir := t.TempDir(); os.WriteFile(dir+"/config", ...)` to test code that
already takes (or could take) an `fs.FS`. It is slower, its directory ordering
is OS-dependent, and it interleaves with cleanup under `-race`. Fix: a
`fstest.MapFS` literal — deterministic, in-memory, nothing to clean up.

### Assuming MapFS can test I/O error handling

Wrong: expecting a `MapFS`-based test to cover the "read failed" branch. It
never will — stored files always read cleanly. Fix: write a fault-injecting
`fs.FS`/`fs.File` wrapper that returns the error you want to cover.

### Comparing error strings instead of errors.Is

Wrong: `strings.Contains(err.Error(), "no such file")`. The message is
OS-specific and brittle. Fix: `errors.Is(err, fs.ErrNotExist)` — the wrapped
`*fs.PathError` makes this the correct, portable check.

### Passing OS-style paths to an fs.FS

Wrong: `fsys.Open("/etc/app.conf")` or `fsys.Open("./conf/../conf/app.yaml")`.
Leading slashes, `./` prefixes, and `..` segments all violate `fs.ValidPath` and
yield `ErrInvalid`. Fix: `fs.FS` paths are always rooted, clean, and
slash-separated — `conf/app.yaml`.

### Never running fstest.TestFS against a custom FS

Wrong: shipping a hand-written `fs.FS` (prefix adapter, union FS) without
validating it. Contract bugs — unsorted `ReadDir`, `Stat` disagreeing with
`Open`, missing invalid-path rejection — ship undetected. Fix: gate every custom
FS through `fstest.TestFS` in a test.

### Relying on Glob or ReadDir order for merge semantics

Wrong: treating the order `fs.Glob` returns as the config precedence order.
`ReadDir` is sorted, but `Glob` follows pattern expansion and its ordering is
not something to build merge semantics on. Fix: `slices.Sort` the matches
explicitly whenever order is load-bearing.

### Forgetting the optional fast-path interfaces

Wrong: a custom `fs.FS` on a hot config path that implements only `Open`. Every
`fs.ReadFile` then pays an extra `Open`+`Stat`. Fix: implement `ReadFileFS`
(and `ReadDirFS` where relevant) so `fs.ReadFile`/`fs.ReadDir` dispatch straight
to your method.

### Dropping the err argument in a WalkDirFunc

Wrong: `func(p string, d fs.DirEntry, err error) error { ... }` that never
checks `err`. A permission or read failure deep in the tree is hidden and the
walk silently returns incomplete results. Fix: `if err != nil { return err }`
at the top of the callback.

### Testing reload logic with time.Sleep and the real clock

Wrong: writing a file, sleeping, rewriting it, and asserting a re-parse. Slow
and flaky. Fix: set `MapFile.ModTime` to explicit instants and swap the entry
for one with a later `ModTime` to simulate an edit — deterministic, instant, no
sleeps.

Next: [01-config-loader-over-fsfs.md](01-config-loader-over-fsfs.md)
