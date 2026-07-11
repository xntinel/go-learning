# Exercise 3: Deliver Rendered Manifests to an Environment Repo with go-git

This is the delivery half of the pipeline: write the rendered desired-state tree
into a local git repository and produce an idempotent, deterministic commit — a
fixed author/committer signature, and a genuine no-op when nothing changed. It
mirrors the environment ("config") repo that Argo CD or Flux then reconciles, and
it runs entirely against a local repository with no network.

## What you'll build

```text
gitdeliver/                  independent module: example.com/gitdeliver
  go.mod                     requires github.com/go-git/go-git/v5
  gitdeliver.go              Deliver; Result; CountCommits; ErrDeliver
  cmd/
    demo/
      main.go                delivers, re-delivers unchanged (no-op), delivers a change
  gitdeliver_test.go         first-commit, idempotent no-op, and change-produces-commit tests; Example
```

Files: `gitdeliver.go`, `cmd/demo/main.go`, `gitdeliver_test.go`.
Implement: `Deliver(repoDir, files, sig, message)` that opens or inits a repo, reconciles the worktree to the desired tree (pruning files no longer present, then writing the rest), stages it, and commits with a fixed `object.Signature` — but only when the worktree is dirty; on clean input it is a no-op returning the current HEAD. Plus `CountCommits`; sentinel `ErrDeliver` wrapped with `%w`.
Test: a first delivery commits and the commit is readable via `CommitObject`/`Log` with the expected message, signature, and files; an unchanged re-delivery is a no-op (clean status, no new commit); a changed file produces a second commit; a manifest dropped from the tree produces a commit that removes that file.
Verify: `go test -race ./...` (`GOFLAGS=-mod=mod` to fetch go-git).

Set up the module:

```bash
mkdir -p gitdeliver/cmd/demo
cd gitdeliver
go mod init example.com/gitdeliver
go get github.com/go-git/go-git/v5@latest
```

### Idempotency is the whole contract

A GitOps controller reconciles continuously. If your delivery step commits
unconditionally, then every pipeline run that produced no change still writes an
empty commit, the environment repo's history fills with noise, and the reconciler
churns. So `Deliver` must be idempotent: write the files, stage them, and *then
check the worktree status*. If `Status().IsClean()` is true, nothing actually
changed on disk relative to the last commit, and `Deliver` returns without
committing — reporting the existing HEAD, not a new hash. Only a dirty worktree
produces a commit. This is the difference between a delivery that settles and one
that never stops.

Setting `AllowEmptyCommits` is not the fix here — that would let the empty commit
through, which is exactly what you do not want. The status check *before*
committing is the mechanism.

### Determinism through a fixed signature

`go-git` records author and committer signatures on every commit, and a signature
carries a timestamp. If you let the timestamp default to `time.Now()`, two commits
of identical content get different hashes, which defeats any downstream comparison.
`Deliver` takes an `object.Signature` from the caller and uses it for both author
and committer, so a caller that passes a fixed `When` gets reproducible commits.
`go-git` also requires an explicit author when no git identity is configured, so
passing the signature is both a determinism choice and a correctness requirement in
a clean CI environment.

### Walking through Deliver

`Deliver` first ensures the directory exists and opens the repository, initializing
a fresh one if the directory is not yet a repo (`git.ErrRepositoryNotExists`). Then
`writeFiles` reconciles the worktree to the desired tree in two steps. It first
*prunes*: `prune` walks the repository directory (skipping `.git`) and removes any
file whose slash-relative path is not a key in `files`, so a manifest dropped from
the desired state is deleted from disk. Without this step `AddWithOptions{All:true}`
(`git add -A`) would never see a deletion — it only stages a removal when the
working-tree file is actually gone — and stale objects would accumulate in the
environment repo forever, the classic GitOps footgun where deleted resources are
never garbage-collected. After pruning, it writes the desired files in sorted order
— sorting matters for reproducible I/O even though git itself is order-insensitive
— creating parent directories as needed. It then stages everything with
`AddWithOptions{All: true}`, which now picks up new files, modified files, and the
deletions the prune step produced. Then it reads `Status()`: a clean status means
the reconcile produced no change, so it returns the current HEAD with
`Committed: false`; a dirty status means it commits with the caller's signature for
both roles and returns the new hash with `Committed: true`. Every failure wraps
`ErrDeliver` so a caller can classify it.

Create `gitdeliver.go`:

```go
package gitdeliver

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// ErrDeliver wraps any failure in the delivery step so callers can classify it.
var ErrDeliver = errors.New("deliver manifests")

// Result reports the outcome of a delivery. Committed is false when the input
// matched the current state exactly, in which case Hash is the existing HEAD (or
// the zero hash if the repository has no commits yet).
type Result struct {
	Committed bool
	Hash      plumbing.Hash
}

// Deliver writes files into the git repository at repoDir, staging and committing
// them with sig as both author and committer. It is idempotent: if the write
// leaves the worktree clean, it makes no commit and returns the current HEAD.
func Deliver(repoDir string, files map[string]string, sig object.Signature, message string) (Result, error) {
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		return Result{}, fmt.Errorf("%w: mkdir %q: %w", ErrDeliver, repoDir, err)
	}

	repo, err := openOrInit(repoDir)
	if err != nil {
		return Result{}, err
	}
	wt, err := repo.Worktree()
	if err != nil {
		return Result{}, fmt.Errorf("%w: worktree: %w", ErrDeliver, err)
	}

	if err := writeFiles(repoDir, files); err != nil {
		return Result{}, fmt.Errorf("%w: write tree: %w", ErrDeliver, err)
	}

	if err := wt.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		return Result{}, fmt.Errorf("%w: stage: %w", ErrDeliver, err)
	}

	status, err := wt.Status()
	if err != nil {
		return Result{}, fmt.Errorf("%w: status: %w", ErrDeliver, err)
	}
	if status.IsClean() {
		head, err := repo.Head()
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return Result{Committed: false}, nil
		}
		if err != nil {
			return Result{}, fmt.Errorf("%w: head: %w", ErrDeliver, err)
		}
		return Result{Committed: false, Hash: head.Hash()}, nil
	}

	hash, err := wt.Commit(message, &git.CommitOptions{
		Author:    &sig,
		Committer: &sig,
	})
	if err != nil {
		return Result{}, fmt.Errorf("%w: commit: %w", ErrDeliver, err)
	}
	return Result{Committed: true, Hash: hash}, nil
}

func openOrInit(dir string) (*git.Repository, error) {
	repo, err := git.PlainOpen(dir)
	if err == nil {
		return repo, nil
	}
	if !errors.Is(err, git.ErrRepositoryNotExists) {
		return nil, fmt.Errorf("%w: open %q: %w", ErrDeliver, dir, err)
	}
	repo, err = git.PlainInit(dir, false)
	if err != nil {
		return nil, fmt.Errorf("%w: init %q: %w", ErrDeliver, dir, err)
	}
	return repo, nil
}

func writeFiles(dir string, files map[string]string) error {
	if err := prune(dir, files); err != nil {
		return err
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		p := filepath.Join(dir, filepath.FromSlash(n))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(files[n]), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// prune removes every file on disk (outside .git) whose slash-relative path is not
// a key in the desired tree, so a manifest dropped from the desired state is
// deleted from the worktree. Running this before the write makes AddWithOptions
// stage the deletion, which is what lets Deliver garbage-collect stale objects
// instead of leaving them to accumulate in the environment repo.
func prune(dir string, files map[string]string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if _, keep := files[filepath.ToSlash(rel)]; !keep {
			return os.Remove(p)
		}
		return nil
	})
}

// CountCommits returns the number of commits reachable from HEAD, or 0 when the
// repository has no commits yet.
func CountCommits(repo *git.Repository) (int, error) {
	head, err := repo.Head()
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	iter, err := repo.Log(&git.LogOptions{From: head.Hash()})
	if err != nil {
		return 0, err
	}
	defer iter.Close()

	n := 0
	err = iter.ForEach(func(*object.Commit) error {
		n++
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}
```

### The demo

The demo delivers a two-file tree to a temp repository, re-delivers the identical
tree (a no-op), then delivers a changed tree (a new commit), printing the
`Committed` flag each time and the final commit count. A fixed signature keeps the
run reproducible.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"example.com/gitdeliver"
)

func main() {
	dir, err := os.MkdirTemp("", "envrepo")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	sig := object.Signature{
		Name:  "CI Bot",
		Email: "ci@example.com",
		When:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}

	v1 := map[string]string{
		"deployment-prod-web.yaml": "kind: Deployment\nspec:\n  replicas: 2\n",
		"service-prod-svc.yaml":    "kind: Service\nspec:\n  ports:\n    - port: 80\n",
	}
	v2 := map[string]string{
		"deployment-prod-web.yaml": "kind: Deployment\nspec:\n  replicas: 5\n",
		"service-prod-svc.yaml":    "kind: Service\nspec:\n  ports:\n    - port: 80\n",
	}

	r1, err := gitdeliver.Deliver(dir, v1, sig, "sync: initial")
	must(err)
	fmt.Println("delivery 1 committed:", r1.Committed)

	r2, err := gitdeliver.Deliver(dir, v1, sig, "sync: no change")
	must(err)
	fmt.Println("delivery 2 committed:", r2.Committed)

	r3, err := gitdeliver.Deliver(dir, v2, sig, "sync: scale to 5")
	must(err)
	fmt.Println("delivery 3 committed:", r3.Committed)

	repo, err := git.PlainOpen(dir)
	must(err)
	n, err := gitdeliver.CountCommits(repo)
	must(err)
	fmt.Println("commits:", n)
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
delivery 1 committed: true
delivery 2 committed: false
delivery 3 committed: true
commits: 2
```

### The tests

`TestFirstDelivery` delivers into a temp dir and reads the commit back with
`CommitObject`: it asserts the message and signature name match, and that the
commit's tree actually contains the delivered file — proof the content was staged,
not just the commit created. `TestIdempotent` re-delivers the identical tree and
asserts `Committed` is false, the returned hash equals the first commit's hash, and
`CountCommits` is still 1 — a true no-op, not an empty commit. `TestChange`
mutates a file and asserts a second commit appears with a different hash. `TestPrune`
delivers a two-file tree, then re-delivers a tree with one file dropped, and asserts
the resulting commit's tree no longer contains the removed file and its content is
gone from the worktree — proof the prune step actually garbage-collects stale
manifests rather than leaving them behind. Each test uses a fixed signature so the
runs are deterministic.

Create `gitdeliver_test.go`:

```go
package gitdeliver

import (
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func fixedSig() object.Signature {
	return object.Signature{
		Name:  "CI Bot",
		Email: "ci@example.com",
		When:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func treeV1() map[string]string {
	return map[string]string{
		"deployment-prod-web.yaml": "kind: Deployment\nspec:\n  replicas: 2\n",
		"service-prod-svc.yaml":    "kind: Service\nspec:\n  ports:\n    - port: 80\n",
	}
}

func TestFirstDelivery(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	res, err := Deliver(dir, treeV1(), fixedSig(), "sync: initial")
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if !res.Committed {
		t.Fatal("first delivery did not commit")
	}

	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	commit, err := repo.CommitObject(res.Hash)
	if err != nil {
		t.Fatalf("CommitObject: %v", err)
	}
	if commit.Message != "sync: initial" {
		t.Errorf("message = %q; want %q", commit.Message, "sync: initial")
	}
	if commit.Author.Name != "CI Bot" {
		t.Errorf("author = %q; want %q", commit.Author.Name, "CI Bot")
	}

	tree, err := commit.Tree()
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if _, err := tree.File("deployment-prod-web.yaml"); err != nil {
		t.Errorf("committed tree missing deployment file: %v", err)
	}
}

func TestIdempotent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	first, err := Deliver(dir, treeV1(), fixedSig(), "sync: initial")
	if err != nil {
		t.Fatalf("first Deliver: %v", err)
	}

	second, err := Deliver(dir, treeV1(), fixedSig(), "sync: no change")
	if err != nil {
		t.Fatalf("second Deliver: %v", err)
	}
	if second.Committed {
		t.Fatal("unchanged re-delivery produced a commit; want no-op")
	}
	if second.Hash != first.Hash {
		t.Errorf("no-op hash = %s; want %s", second.Hash, first.Hash)
	}

	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	n, err := CountCommits(repo)
	if err != nil {
		t.Fatalf("CountCommits: %v", err)
	}
	if n != 1 {
		t.Errorf("commit count = %d; want 1", n)
	}
}

func TestChange(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	first, err := Deliver(dir, treeV1(), fixedSig(), "sync: initial")
	if err != nil {
		t.Fatalf("first Deliver: %v", err)
	}

	changed := treeV1()
	changed["deployment-prod-web.yaml"] = "kind: Deployment\nspec:\n  replicas: 5\n"

	second, err := Deliver(dir, changed, fixedSig(), "sync: scale to 5")
	if err != nil {
		t.Fatalf("second Deliver: %v", err)
	}
	if !second.Committed {
		t.Fatal("changed delivery did not commit")
	}
	if second.Hash == first.Hash {
		t.Error("changed delivery reused the first commit hash")
	}

	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	n, err := CountCommits(repo)
	if err != nil {
		t.Fatalf("CountCommits: %v", err)
	}
	if n != 2 {
		t.Errorf("commit count = %d; want 2", n)
	}
}

func TestPrune(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if _, err := Deliver(dir, treeV1(), fixedSig(), "sync: initial"); err != nil {
		t.Fatalf("first Deliver: %v", err)
	}

	// Drop the service manifest from the desired tree.
	shrunk := map[string]string{
		"deployment-prod-web.yaml": treeV1()["deployment-prod-web.yaml"],
	}
	res, err := Deliver(dir, shrunk, fixedSig(), "sync: drop service")
	if err != nil {
		t.Fatalf("second Deliver: %v", err)
	}
	if !res.Committed {
		t.Fatal("dropping a manifest did not produce a commit")
	}

	// The removed file must be gone from the worktree.
	if _, err := os.Stat(dir + "/service-prod-svc.yaml"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("service manifest still on disk: stat err = %v", err)
	}

	// The removed file must be absent from the committed tree.
	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	commit, err := repo.CommitObject(res.Hash)
	if err != nil {
		t.Fatalf("CommitObject: %v", err)
	}
	tree, err := commit.Tree()
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if _, err := tree.File("service-prod-svc.yaml"); !errors.Is(err, object.ErrFileNotFound) {
		t.Errorf("committed tree still contains dropped service: err = %v", err)
	}
	if _, err := tree.File("deployment-prod-web.yaml"); err != nil {
		t.Errorf("committed tree missing retained deployment: %v", err)
	}
}

func TestDeliverWrapsErr(t *testing.T) {
	t.Parallel()
	// A path that cannot be created (a file where a directory is expected) forces
	// a wrapped ErrDeliver.
	dir := t.TempDir() + "/afile"
	if err := os.WriteFile(dir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Deliver(dir+"/sub", treeV1(), fixedSig(), "msg")
	if !errors.Is(err, ErrDeliver) {
		t.Fatalf("err = %v; want ErrDeliver", err)
	}
}

func ExampleDeliver() {
	dir, _ := os.MkdirTemp("", "envrepo")
	defer os.RemoveAll(dir)

	first, _ := Deliver(dir, treeV1(), fixedSig(), "sync: initial")
	again, _ := Deliver(dir, treeV1(), fixedSig(), "sync: no change")
	fmt.Println(first.Committed, again.Committed)
	// Output: true false
}
```

## Review

Delivery is correct when it commits exactly once per real change. `TestFirstDelivery`
proves the commit is real by reading its message, signature, and tree back;
`TestIdempotent` proves the no-op path by checking that a second identical delivery
returns `Committed: false`, the same hash, and leaves the commit count at one; and
`TestChange` proves a mutated file yields a second, distinct commit; and
`TestPrune` proves a manifest dropped from the desired tree is removed from both the
worktree and the committed tree. Read those together and they pin the contract: the
committed tree always equals the desired tree, one commit per change, zero commits
per non-change.

The mistake that breaks GitOps quietly is committing unconditionally — it produces
empty commits on unchanged input and makes the reconciler churn. The fix is the
status check before committing, not `AllowEmptyCommits`. The second mistake is
letting the signature timestamp float to `time.Now()`, which makes otherwise
identical commits hash differently; pass a fixed `object.Signature`. The third,
subtler mistake is skipping the prune step and relying on `git add -A` alone to
remove deleted manifests: `git add -A` only stages a deletion when the file is
already gone from disk, so without an explicit prune the removed objects linger and
are never garbage-collected. Because this lesson depends on go-git, resolve it with
`GOFLAGS=-mod=mod`, and pin the stable `v5` line rather than the `v6` alpha.

## Resources

- [`github.com/go-git/go-git/v5`](https://pkg.go.dev/github.com/go-git/go-git/v5) — `PlainInit`, `PlainOpen`, `Worktree`, `AddWithOptions`, `Status`, `Commit`, `CommitOptions`, `Log`.
- [`github.com/go-git/go-git/v5/plumbing/object`](https://pkg.go.dev/github.com/go-git/go-git/v5/plumbing/object) — `Signature`, `Commit`, and `Tree.File`.
- [go-git examples](https://github.com/go-git/go-git/tree/master/_examples) — canonical commit, log, and status flows in pure Go.
- [Argo CD — apps of apps and rendered manifests](https://argo-cd.readthedocs.io/en/stable/user-guide/) — how an environment repo is reconciled after delivery.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-rendered-manifests-drift.md](02-rendered-manifests-drift.md) | Next: [../06-multi-cloud-blob-storage-gocloud/00-concepts.md](../06-multi-cloud-blob-storage-gocloud/00-concepts.md)
