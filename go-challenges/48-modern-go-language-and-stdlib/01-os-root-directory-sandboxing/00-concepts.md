# Directory-Confined Filesystem with os.Root — Concepts

User-supplied path components are one of the oldest ways to read or overwrite a file the program never meant to expose. `filepath.Join(base, userPath)` does not stop `../../../../etc/passwd`, and validating a path with `filepath.EvalSymlinks` and *then* opening it leaves a TOCTOU window where an attacker swaps a component for a symlink in between. Go 1.24 added `os.Root` (expanded in 1.25) to make confinement a property of the filesystem handle itself: every operation resolves relative to a directory file descriptor and rejects any component that would escape. This file is the conceptual foundation — the model, the threat, the failure modes, the platform limits, and when not to reach for it. Read it once and you will have everything you need to build the confined `safestore` package in the exercise that follows.

## Concepts

### The confinement model

`os.OpenRoot(dir)` opens `dir` and keeps a handle to it (a file descriptor on Unix, a directory handle on Windows). Every method on the returned `*os.Root` resolves names relative to that handle, one component at a time, using the `openat` family of syscalls on Unix. Because resolution is anchored to the descriptor rather than re-walking a string path, the kernel — not your string sanitizing — enforces that the result stays beneath the root. The handle also pins the directory: if it is renamed or moved, the root keeps referring to the same inode in its new location.

The security-relevant methods are `Open`, `OpenFile`, `Create`, `ReadFile`, `WriteFile`, `Mkdir`, `MkdirAll`, `Remove`, `Stat`, `Lstat`, `Rename`, `FS`, and `OpenRoot` (a nested, independently confined root). `ReadFile`/`WriteFile`/`Rename`/`MkdirAll` and the metadata setters arrived in Go 1.25; `Open`/`Create`/`Mkdir`/`Stat`/`FS` and the constructor shipped in 1.24.

A typical use is a handful of lines, with no path sanitizing anywhere in sight:

```go
root, err := os.OpenRoot(baseDir)
// ...
defer root.Close()
data, err := root.ReadFile(userSuppliedName) // confined to baseDir
```

### What it rejects, and the error you actually get

A name escapes when, after resolving `.` and `..` and any symlinks, it would land outside the root. Three cases are rejected: a relative `..` chain that climbs above the root, a symlink whose target resolves outside the root, and any absolute symlink target (absolute targets are always refused, even if they happen to point back inside).

The rejection surfaces as an `*os.PathError` whose wrapped error is the unexported sentinel with the message `path escapes from parent` — for example `openat escape.lnk: path escapes from parent`. There is **no exported sentinel** for this, so you cannot write `errors.Is(err, someEscapeError)`. Crucially, `errors.Is(err, os.ErrNotExist)` is *false* for an escape and *true* for a genuinely missing file, so a denied escape and a 404 are distinguishable by that check, but the escape itself has no stable typed identity. The right contract for callers is therefore "any error means the access was denied" — confinement is safe by default, and you should not be classifying paths yourself anymore.

### Symlinks: followed inside, rejected outside

`os.Root` is not a no-symlinks mode. A symlink whose resolved target stays under the root is followed normally; only targets that escape are refused. That keeps legitimate layouts (a `latest` link to a versioned directory inside the tree) working while still blocking `evil -> /etc/passwd`. This is exactly what the exercise asserts: `inside.lnk -> ok.txt` reads fine, while `escape.lnk -> ../secret.txt` and an absolute symlink both fail.

### Why the old check-then-open is unsafe (TOCTOU)

The classic pattern resolves the path, checks it is inside the base, then opens it. Between the check and the open, an attacker who can write to the directory replaces a path component with a symlink, and the open follows it. `os.Root` removes the gap: there is no separate check; the confinement is evaluated as part of the same anchored resolution that performs the operation, so there is no moment when a swapped component can be followed.

### One-shot access with os.OpenInRoot

Sometimes you do not want to keep a `Root` around — you just need to open one attacker-named file safely. `os.OpenInRoot(dir, name)` does exactly that: it is equivalent to `OpenRoot(dir)` followed by opening `name`, applying the same escape checks, but it hands you a plain `*os.File` and manages the root internally, so there is no handle for you to track. The trade-off is the mirror image of `OpenRoot`: convenient for a single open, wasteful if you call it in a loop over the same directory (each call re-opens the root), where a reused `*os.Root` is cheaper. Reach for it for a single confined open; reach for a `Root` when you will do several operations under the same directory and want to amortize the handle.

### The io/fs bridge: Root.FS()

`root.FS()` returns an `fs.FS` rooted at the directory — and it implements `fs.StatFS`, `fs.ReadFileFS`, `fs.ReadDirFS`, and (since Go 1.25) `fs.ReadLinkFS`. That means any `io/fs` consumer — `fs.ReadDir`, `fs.WalkDir`, `fs.Glob`, a `html/template` loader — runs *inside the confinement* when you hand it `root.FS()`, even though that code knows nothing about `os.Root`. A walker built on `root.FS()` cannot wander out of the tree, so you can hand it to existing, traversal-unaware code and inherit the guarantee.

### Stat versus Lstat

`Stat` follows a trailing symlink (still subject to the escape check, so a link pointing outside fails); `Lstat` reports on the link itself without following it. Use `Lstat` when you need to *detect* a symlink — for example, to decide whether to descend — rather than resolve through it.

### The archetypal use: safe archive extraction (zip-slip)

The reason `os.Root` exists is extraction. Unpacking a zip or tar means writing files whose names come straight from the archive, and a malicious archive embeds names like `../../etc/cron.d/x` ("zip-slip") to escape the extraction directory. The naive `filepath.Join(dir, entry.Name)` does nothing to stop it: `Join` cleans the path lexically but still happily produces a path outside `dir`. Extracting through a `Root` turns every such entry into a rejected write, and creating parent directories with `Root.MkdirAll` is confined for the same reason — a `../` in a directory component is refused too. The whole unpack becomes safe by construction, with no per-name validation. The `Extract` method in the exercise is the concrete version of this.

### Platform guarantees and limits

The guarantee is strongest on Unix (`openat`, with an `openat2`/`RESOLVE_BENEATH` fast path on newer Linux) and Windows (a held directory handle that also blocks the root from being renamed or deleted, and refuses reserved device names like `NUL`). On `js`/`wasm` the protection is weaker and can still be subject to symlink TOCTOU, because the platform lacks the `openat` family. And `os.Root` deliberately does **not** stop everything: it does not block crossing mount points, Linux bind mounts, `/proc` magic files, or Unix device files. It confines *path resolution*, not *what a confined path can name*. Treat it as a strong defense against traversal, not a full sandbox.

### When not to use it

If the path is fully under your control (a constant, or a name you generated), plain `os` calls are fine and clearer. Reach for `os.Root` exactly when a path — or any component of it — comes from outside the program: request parameters, archive entry names (zip-slip), uploaded filenames, config that names files.

## Common Mistakes

### Sanitizing the string instead of anchoring the open

Wrong: `clean := filepath.Clean(userPath); if strings.HasPrefix(clean, base) { os.Open(clean) }`. `filepath.Clean` collapses `..` lexically but does not resolve symlinks, and the prefix check is fooled by a symlinked component.

Fix: open a root once with `os.OpenRoot(base)` and call `root.Open(userPath)`. Resolution is anchored to the directory handle, so no string check is needed.

### Checking the path, then opening it (TOCTOU)

Wrong: `EvalSymlinks` + prefix check, then a later `os.Open`. An attacker swaps a component for a symlink in the gap.

Fix: `os.Root` evaluates confinement as part of the operation; there is no gap.

### Extracting archives with filepath.Join

Wrong: `dst := filepath.Join(dir, entry.Name); os.WriteFile(dst, ...)`. A `../../` entry name escapes `dir`; this is the zip-slip vulnerability.

Fix: extract through a `Root` (or `os.OpenInRoot`) so escaping entry names are rejected, as `Extract` does.

### Expecting an exported error to test for

Wrong: `if errors.Is(err, os.ErrEscape) { ... }`. No such sentinel exists; the escape error wraps an unexported value, and `errors.Is(err, os.ErrInvalid)` does not match it either.

Fix: treat any non-nil error as "denied". If you must tell a missing file from a denied escape, note that `errors.Is(err, os.ErrNotExist)` is true only for the former.

### Assuming os.Root is a full sandbox

Wrong: relying on `os.Root` to block `/proc`, bind mounts, or device files.

Fix: it confines path resolution, not what a confined path may name. Combine it with OS-level isolation (namespaces, seccomp) when you need those guarantees.

---

Next: [01-confined-filesystem.md](01-confined-filesystem.md)
