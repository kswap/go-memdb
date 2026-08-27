# ART evaluation — status and next steps

Working notes for the `art-support` branch. The question was whether an Adaptive
Radix Tree would be a better storage engine for go-memdb than the immutable
radix tree it uses today. Written so the work can be picked up cold.

**Status:** the cheap win is built, measured and committed. The ART question is
answered in principle but one blocker is unresolved — and it turns out not to be
a property of ART at all.

All measurements: Apple M4 Max, Go 1.25.7, darwin/arm64. Re-measure before
trusting any comparison on other hardware.

---

## TL;DR

1. **Over half of go-memdb's tree memory was watch machinery for watchers that
   did not exist.** Fixed. Memory per index entry is down **56–60%**, heap
   objects down **37–40%**, with no semantic change.
2. **A good ART beats even the improved tree on nearly everything** — point
   lookups, inserts, updates, memory. This contradicts the only existing
   memdb-on-ART port, whose numbers are not trustworthy.
3. **The one thing ART loses is prefix scan, which is memdb's dominant read —
   and the cause is a single heap allocation in a pull iterator, not the tree.**
   Driven by a push iterator the same ART beats our tree with zero allocations.
   That is the next thing to test.

---

## What is on this branch

| Commit | What it delivers |
|---|---|
| `de3c5bb` | `CLAUDE.md` — architecture and workflow notes |
| `3726e01` | `bench_test.go` — macro benchmark harness at the memdb API level |
| `15cdd41` | Single-pass UUID index key decoding |
| `5d5fb0b` | `internal/iradix` fork with lazily allocated watch channels |
| `440fa8f` | `bench/` — tree-level A/B against a reference persistent ART |

Everything is green: `go build ./...`, `go vet ./...`, `gofmt`, and
`go test -race -count=2 ./...` including all 2,300 lines of the upstream tree's
own tests, carried over with the fork.

---

## Findings

### The workload, measured not assumed

Counted from `consul@v1.22.5/agent/consul/state`:

| API | sites | | API | sites |
|---|---|---|---|---|
| `tx.Get(` prefix scan | **138** | | `Txn(false)` / `Txn(true)` | 114 / 6 |
| `.First(` / `FirstWatch(` | 115 / 30 | | `_prefix` index use | 48 |
| `DeletePrefix(` / `LongestPrefix(` | 2 / 1 | | `NewWatchSet(` across `agent/` | **243** |
| **`LowerBound` / `ReverseLowerBound` / `GetReverse`** | **0** | | | |

Reads outnumber writes ~19:1. Prefix scans and `First` are the whole workload.
The entire range-seek API is unused. Watches are everywhere.

Not yet done: repeat this for Vault and Nomad (neither is in the local module
cache; needs a checkout).

### Baseline, and what the cheap fix bought

Memory per index entry, 1M rows, measured with `TestFootprint`:

| shape | before | after | Δ bytes | Δ objects |
|---|---|---|---|---|
| uuid | 401.2 B / 5.39 | 166.6 B / 3.29 | −58.5% | −39.0% |
| prefixstr | 416.5 B / 5.36 | 180.0 B / 3.25 | −56.8% | −39.4% |
| word | 445.6 B / 6.41 | 182.7 B / 4.06 | −59.0% | −36.7% |
| sequint | 371.8 B / 5.02 | 147.4 B / 3.01 | −60.4% | −40.0% |
| randuint | 400.7 B / 5.36 | 166.2 B / 3.27 | −58.5% | −39.0% |
| compound | 423.9 B / 5.61 | 185.5 B / 3.48 | −56.2% | −38.0% |

Write path also dropped 16% of bytes and 17% of allocations. Reads and scans
unchanged.

The proof that this was the right target: before the fix, running the *same*
write against a snapshot (mutation tracking off) versus the primary saved only
**3.1% of bytes and 3.4% of allocations** — because the channels were allocated
on the way in regardless of whether anyone watched.

Also fixed: `UUIDFieldIndex.parseString` was 40% of all allocated objects in a
UUID point lookup. Now 80.0→39.2 ns, 48→16 B, 2→1 allocs; `First` on a UUID
index went 199.2→158.9 ns.

### Unfixed: the notify cliff

200,000-row two-index table, updating N rows in one transaction:

| rows touched | commit time | allocs/op |
|---|---|---|
| 1,000 | 90.0 µs | 34 |
| 2,000 | 209 µs | 34 |
| **4,000** | **37.1 ms** | **1,118,604** |
| 40,000 | 50.7 ms | 1,118,604 |

**177× slower and 32,900× more allocations** between 2k and 4k touched rows.
Note row four: 40,000 costs the same as 4,000, because once the fallback engages
the work is proportional to the **whole tree**, not to what changed.

Mechanism confirmed by profile: past 8,192 tracked slots, `iradix` discards its
tracking set and `slowNotify` merge-walks both trees, building two strings per
node via `rawIterator.Next` — the top allocator in the process at 22.5% of all
objects.

This is an ordinary batched write on the Raft apply path. Lazy channels do not
help; the walk itself is the cost. **This is the worst thing found and it is
still there.**

### ART versus the improved tree

Tree-level, 200k keys, `cilium/statedb/part` as the ART:

| Operation | Winner | Margin |
|---|---|---|
| Point lookup, high-entropy keys | **ART** | −57% to −68% |
| Point lookup, prefix-shared keys | even | ~0% |
| Insert (batched) | **ART** | −41% to −52% time, −45% bytes |
| Update | **ART** | −20% to −40% time, −52% bytes |
| Memory per key | **ART** | −32% to −38% |
| Full iterate, high-entropy | **ART** | −30% to −36% |
| Full iterate, dense / prefix-shared | radix | ART +17% to +60% |
| **Prefix scan (pull iterator)** | **radix** | ART +165% time, 896 B vs 49 B |
| Prefix scan (push iterator) | **ART** | −12%, **zero** allocations |

---

## The open question, and why it is tractable

Prefix scan is the only loss, and it is memdb's dominant read. The cause is one
line in `part`'s `Next()`:

```go
it.edges = make([][]*header[T], 1, 32)   // 768 B → 896 B size class, per iterator
```

The traversal stack must survive between `Next()` calls, so it goes on the heap,
preallocated at 32 slots. `All()` does the identical walk on the Go stack and
beats our radix tree with **zero** allocations.

Our radix tree allocates only 49 B for the same scan because its stack grows
lazily from nil, reaching actual tree depth (~4–8) rather than a fixed 32.

memdb's public `ResultIterator` is `Next()`-based, so memdb would pay the 896 B.
But this looks like an implementation choice, not a structural property of ART.

---

## Next steps, in priority order

### 1. Test the iterator hypothesis — half a day, decisive

Vendor `part` locally, change `Next()` to grow `it.edges` lazily (or inline a
small depth-bounded array), re-run `BenchmarkPrefixScan`.

- If prefix scan comes level or better, **ART wins on every axis** and the case
  for adopting it is made.
- If it does not, the loss is structural and ART is a much harder sell for this
  workload.

Either way this is the highest-information experiment remaining, and it costs
almost nothing.

```sh
cd bench && go test -run '^$' -bench BenchmarkPrefixScan -benchtime=200000x -benchmem -count=6 .
```

### 2. Fix the notify cliff — worst unfixed bug

Two candidate approaches:

- **Raise the tracking limit.** A tracked entry is now an 8-byte `*watchable`
  pointer rather than a channel reference, so 8,192 is far more conservative
  than it needs to be. Cheap, but only moves the cliff.
- **Make `slowNotify` stop building path strings.** It compares tree positions
  via `rawIterator.Path()`, which concatenates a string per node. Comparing node
  identity or a reusable byte buffer would remove the allocations entirely.

Benchmark already exists: `BenchmarkNotifyCliff` in `bench_test.go`.

### 3. Bulk load path

Consul, Nomad and Vault restore from Raft snapshots by inserting millions of
rows one at a time, each a full path copy. A `BulkInsert(sortedPairs)` that
builds bottom-up in one pass, skipping copy-on-write and watch tracking, should
be a step change in restore time. Works on the fork; independent of the ART
decision. Both VART and PART found this necessary.

### 4. Remaining fork items (not yet done)

From the original plan, none of these are implemented:

- Replace the `simplelru` writable-node cache with a `txnID uint64` stamp in the
  node. Removes 2 of ~6 allocations per copied node and drops the
  `hashicorp/golang-lru` dependency. **Watch out:** bump the txn id before
  handing out any iterator or `Clone()`, or in-place mutation of same-txn nodes
  corrupts a live iterator, since iterators alias children arrays.
- Fix the edge-array double allocation: `writeNode` copies with `cap == len`, so
  `addEdge`'s `append` always reallocates.
- Replace `expandedParents` (a map insert and delete per internal node in
  `reverse_iter.go`) with an explicit path stack. Reverse iteration currently
  costs 2.2× forward.

### 5. Then decide: adopt `part`, or write our own

Only worth asking if step 1 comes out well.

- **Adopting `part`** needs reverse iteration, `LongestPrefix` and
  `DeletePrefix` written — all of which Consul barely uses (0, 1 and 2 sites).
  Apache-2.0, actively maintained. Note it is a package inside a rival in-memory
  database, not a standalone library.
- **Writing our own** is ~3,500–4,500 lines plus ~3,000 of tests, realistically
  6–12 weeks. Use `part` as the reference design. The research notes on node
  sizing, watch design and copy-on-write are in the published analysis (link
  below).

---

## Reproducing the measurements

```sh
# memdb-level benchmarks
go test -run '^$' -bench . -benchmem -count=10 -timeout 60m .

# steady-state memory per index entry (builds multi-million-row tables)
go test -run TestFootprint -v -timeout 30m -footprint .

# the notify cliff
go test -run '^$' -bench BenchmarkNotifyCliff -benchtime=5x -benchmem .

# tree-level ART comparison
cd bench
go test -run '^$' -bench . -benchmem -count=6 .
go test -run TestTreeFootprint -v -footprint .
```

For an A/B against `main`, the harness only touches memdb's public API, so the
identical source compiles on both sides:

```sh
git worktree add ../memdb-baseline main
(cd ../memdb-baseline && go test -run '^$' -bench . -benchmem -count=10) > base.txt
go test -run '^$' -bench . -benchmem -count=10 > cand.txt
benchstat base.txt cand.txt
```

`benchstat` currently requires Go 1.26; with Go 1.25 compare medians manually
and interleave the two sides to cancel thermal drift.

---

## Things that were wrong, so they are not redone

- **`First`/`Last` "wasting" a watch channel cost nothing** under the original
  eager design — `Node.Get` just called `GetWatch` and discarded the result.
  Making channels lazy *created* that cost, which is why `wantWatch` is now
  threaded through the descent. Read-path allocations went 2 → 6 before this was
  caught.
- **`indexPath` does not allocate on the read path.** Escape analysis
  stack-allocates it in `readableIndex`; it only reaches the heap on the commit
  path. Lower value than the plan assumed — still unfixed, still minor.
- **A `switch`-based hex decoder was faster in isolation but 13% slower in its
  caller.** `encoding/hex` uses a 256-byte reverse lookup table for a reason. A
  control on a key shape that never calls the function proved the regression was
  real rather than code-layout noise.
- **Chunking wide ART nodes was proposed and dropped.** No production persistent
  ART does it; it adds an indirection to the hottest path to save a copy that
  per-transaction ownership stamping already avoids.
- **`part` does allocate its watch channels lazily.** An earlier reading said
  otherwise — it allocates the `watchState` struct on clone, but the channel
  inside is created by CAS on first use.
- **A build-from-empty transaction never reaches the notify fallback**, because
  channels are only tracked when an already-published node is copied. The cliff
  benchmark has to mutate an existing tree.
- **The abandoned `origin/using-art` branch is not a usable data point.** Its
  interface-typed child pointers make ART's memory look worse than it is, and
  its `First` bypasses `FirstWatch` and drops the watch channel, making its reads
  look better. Its published claim that ART loses on updates and UUID lookups is
  contradicted by the measurements here.

---

## References

- Published analysis, with the architecture walkthrough, production-ART survey
  and all baseline numbers:
  https://claude.ai/code/artifact/6493c3d3-f286-4914-b566-2ea96ec3b402
- Branch: https://github.com/kswap/go-memdb/tree/art-support
- `cilium/statedb/part` — the reference persistent ART, pinned to a main-branch
  pseudo-version in `bench/go.mod` because released v0.8.4 lacks the
  non-watching `Get`.
- Prior art: Consul PR #20655 (ART for V2 indexes, 2.4% faster, closed
  unmerged), `origin/using-art` (abandoned Aug 2024), `banks/go-cmpeqepi8`
  (2019, found SIMD Node16 search loses to brute force in Go).
