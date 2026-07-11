# Weak Pointers and runtime.AddCleanup — Concepts

A cache of large, shareable objects — parsed configs, decoded images, compiled templates — wants two things that pull against each other: while a value is in use it should be shared, not re-decoded; once nothing uses it, it should be freed, not pinned by the cache forever. A normal `map[K]*V` cannot do this: the map's pointer is a strong reference, so the value never becomes unreachable and the cache leaks. Go 1.24 added `weak.Pointer[T]` (reference a value without keeping it alive) and `runtime.AddCleanup` (run a function after a value is reclaimed) to build exactly this kind of self-pruning cache. This file is the conceptual foundation: read it once and you will have everything you need to build a self-pruning weak-value cache and a cleanup-backed resource as independent, self-contained Go modules, and to understand why `AddCleanup` replaces the old `runtime.SetFinalizer`.

## Concepts

### Weak pointers reference without retaining

`weak.Make(ptr *T) weak.Pointer[T]` creates a weak reference. `Pointer.Value()` returns the original `*T` while the object is still reachable through some strong reference, and `nil` once the object has been collected. An object pointed to *only* by weak pointers is not considered reachable, so the GC is free to reclaim it. Weak pointers also compare by object identity and keep comparing equal even after the object is gone — which is what lets canonicalization maps (the `unique` package is built on this) use them as stable keys.

### AddCleanup runs after reclamation

`runtime.AddCleanup(ptr *T, cleanup func(S), arg S) Cleanup` registers `cleanup(arg)` to run, in a separate goroutine, some time after `ptr` becomes unreachable. The returned `Cleanup` has a `Stop()` to cancel it. The canonical use is releasing an external resource a wrapper object owns. We also use it to delete the now-dead map entry from the cache.

Three rules decide whether it works:

- `ptr` must not be reachable from `cleanup` or `arg`. If it is, the object stays alive and the cleanup never runs. As a guard, `AddCleanup` panics if `arg == ptr`.
- Cleanups run concurrently — with your goroutines and with each other — and in no defined order. The cleanup body must be safe to run on another goroutine (here, it locks the cache mutex).
- It is best-effort: a cleanup is not guaranteed to run before the program exits, and not guaranteed at all for a zero-size `T`. Never rely on it for correctness.

### Why AddCleanup, not SetFinalizer

`runtime.SetFinalizer` has sharp edges that `AddCleanup` removes: a finalizer can *resurrect* the object (the finalizer receives the live pointer), so collection takes an extra GC cycle; only one finalizer is allowed per object; and a finalizer on an object that is part of a reference cycle is *not guaranteed to run* (and the cycle is not guaranteed to be collected). `AddCleanup` passes an arbitrary `arg` rather than the object itself (so it cannot resurrect it), allows many cleanups per object, works on interior pointers, and runs even on cycles. For new code, prefer `AddCleanup`.

### The Cleanup handle: Stop() the safety net

`AddCleanup` returns a `runtime.Cleanup` whose `Stop()` cancels the pending cleanup. This is what makes it usable as a *backstop* for resource release: a wrapper attaches a cleanup that frees its handle if the caller forgets to `Close`, and `Close` calls `cleanup.Stop()` so the release happens exactly once — in `Close` on the happy path, or in the cleanup only if `Close` was skipped. The cleanup-backed resource module builds this pattern and tests both paths.

`Stop()` carries a precondition worth stating: it has no effect once the cleanup has already been queued to run, and it only *guarantees* cancellation if the pointer passed to `AddCleanup` is still reachable across the `Stop()` call. In the `Resource` pattern that holds automatically — `Close` is a method on the very object the cleanup is attached to, so the receiver keeps it reachable through the `Stop()` — but if you ever call `Stop()` from somewhere that no longer references the object, the guarantee lapses.

### Weak pointer identity and interior pointers

Two `weak.Pointer` values compare equal exactly when the pointers they were made from were equal, and they keep comparing equal even after the object is reclaimed. A weak pointer maps to an object *and an offset*, so weak pointers to two different fields of the same struct are not equal. That identity property — not just the nil-after-collection behavior — is what makes weak pointers usable as stable keys in canonicalization maps.

### Tiny pointer-free objects may never be collected individually

There is a caveat that shapes how you *test* this, and it is documented for both `weak.Pointer` and `AddCleanup`: the runtime may batch tiny, pointer-free objects (such as a bare `int`) into a shared allocation, so an individual one of them may never become collectible on its own. The practical consequence is that a weak pointer to a lone `int` might never go `nil` and a cleanup attached to it might never fire. Any test that wants to *observe* collection must therefore use a value that is allocated on its own — a non-tiny, pointer-containing value such as a `[]byte` of a kilobyte — so dropping the only strong reference really does make it individually reclaimable. This is why the modules' garbage-collection tests use `[]byte` values rather than `int`.

### The self-pruning cache pattern, and its honest limits

The cache stores `map[K]weak.Pointer[V]`. `Get` returns the live `*V` if the weak pointer still resolves, and otherwise reloads. `AddCleanup` deletes the entry once the value is collected. Because cleanups are asynchronous and best-effort, the map may briefly hold a *stale* entry whose value is already gone — so `Get` must treat a `nil` `Value()` as a miss and reload. The weak pointer provides correctness (a stale entry is never mistaken for a live value); the cleanup provides only memory hygiene (eventually removing the empty slot). Do not use this for lifetime guarantees: if a value *must* stay alive, hold a strong reference; if a resource *must* be released, use `defer Close`, not a cleanup.

## Common Mistakes

### Using a strong map and wondering why memory grows

Wrong: `map[K]*V` as a cache. The map pins every value forever.

Fix: `map[K]weak.Pointer[V]`, and reload when `Value()` returns `nil`.

### Treating a weak hit as guaranteed live

Wrong: `v := c.items[key].Value(); use(v)` without a `nil` check. The value may already be collected.

Fix: always check `Value() != nil` and reload on a miss, as `Get` does.

### Making the object reachable from its own cleanup

Wrong: `runtime.AddCleanup(obj, func(o *T){...}, obj)`. Passing the object as `arg` (or capturing it in the cleanup) keeps it alive forever, so the cleanup never runs — and `arg == ptr` panics outright.

Fix: pass only the data the cleanup needs (here, the `key` or a released flag), never the object.

### Relying on a cleanup for correctness or resource release

Wrong: closing a file or releasing a lock only in an `AddCleanup`. Cleanups are best-effort and may not run before exit.

Fix: release resources with `defer Close`. Use a cleanup only as a backstop or for memory hygiene, as the cache does.

### Testing collection with a tiny pointer-free value

Wrong: a `map[K]weak.Pointer[int]` test that drops its only `*int`, calls `runtime.GC()`, and asserts the weak pointer is now `nil`. The runtime may have batched that `int` into a shared allocation, so it never becomes individually collectible and the assertion flakes.

Fix: test collection with a value allocated on its own — a `[]byte` of a kilobyte — so dropping the strong reference really does make it reclaimable.

---

Next: [01-weak-value-cache.md](01-weak-value-cache.md)
