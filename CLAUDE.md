# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

`go-memdb` is a single-package Go library (`package memdb`, module `github.com/hashicorp/go-memdb`) providing an
in-memory database with MVCC, transactions, rich indexing, and watches. It is built on top of
[go-immutable-radix](https://github.com/hashicorp/go-immutable-radix) — that dependency's semantics drive most of the
design here. There is no binary to build; the only non-library code is `watch-gen/`, a code generator.

## Commands

```sh
go test ./...                                   # full test suite
go test -run TestTxn_Insert_First ./...          # single test
go test -run 'TestTxn_.*' ./...                  # test subset by regex
go test -bench . -run '^$' ./...                 # benchmarks (BenchmarkWatch, BenchmarkCompoundMultiIndex_FromObject, ...)
go test -race ./...                              # concurrency-sensitive code; worth running for txn/watch changes

go fmt ./...    # CI fails if this reports any file
go vet ./...
golangci-lint run   # config enables only errcheck

go generate ./...   # regenerates watch_few.go from watch-gen/main.go
```

CI (`.github/workflows/test-and-build.yml`) runs fmt/vet/golangci-lint plus tests on Go 1.23 (the version in `go.mod`),
`oldstable`, and `stable`, with `gotestsum ... -p 2 -cover`.

Every `.go` file carries a two-line copyright/SPDX header; new files need it or the compliance workflow will add it.

## Architecture

### Storage layout

A `MemDB` holds a single `unsafe.Pointer` to a root `iradix.Tree`. That root is a radix tree *of indexes*: the key is
`"<table>.<index>"` (`indexPath` in `memdb.go`) and the value is itself an `*iradix.Tree` mapping encoded index keys to
the stored objects. So there are two levels of radix tree, and "the database" is one atomically-swappable pointer.

Objects are stored by reference and never copied. Mutating an inserted object in place corrupts the DB — including
after deletion, since older snapshots may still reference it. Updates must insert a fresh copy.

### Transactions and MVCC (`txn.go`)

`db.Txn(write)` starts a transaction over `rootTxn` (a radix txn on the current root). Writes take `db.writer`, a
single mutex — one writer at a time, unbounded concurrent readers, no reader locking at all.

- `writableIndex(table, index)` lazily opens and caches per-`(table,index)` radix sub-transactions in `txn.modified`.
- `readableIndex` returns a `Clone()` of a modified sub-txn when one exists, so reads inside a write txn see the
  txn's own uncommitted writes. This clone is why a `ResultIterator` sees a point-in-time snapshot: mutations after
  the iterator is created are invisible to it (documented on the `ResultIterator` interface).
- `Commit` commits each sub-txn with `CommitOnly`, writes the results into `rootTxn`, atomically swaps `db.root`, and
  only *then* calls `Notify()` on each sub-txn — so watchers waking up always observe the new root. Finally it runs
  `txn.after` deferred funcs (LIFO) and unlocks the writer.
- `Abort` discards everything and unlocks. Both are no-ops on read txns or an already-finished txn (`rootTxn == nil`).
- `db.Snapshot()` returns a `MemDB` clone sharing the current root with `primary: false`. Non-primary DBs disable
  `TrackMutate`, so writes to a snapshot never fire watches on the primary.

### Indexing (`index.go`, `schema.go`)

A schema is `DBSchema` → `TableSchema` → `IndexSchema`; `Validate()` enforces that map keys match `Name` fields and
that every table has a unique `"id"` index backed by a `SingleIndexer`.

An `Indexer` builds a lookup key from query args (`FromArgs`); it must also implement `SingleIndexer`
(one key per object) or `MultiIndexer` (many keys per object, all pointing at the same object). `PrefixIndexer` is
optional and enables prefix scans.

Two conventions run through the query path:

- **Non-unique indexes**: `Insert` appends the primary key bytes to each index value, making every key unique in the
  underlying tree. This is invisible to callers but explains why non-unique index keys are prefixes, and why deletes
  must recompute the same suffixed key from the *existing* object.
- **`_prefix` suffix**: `getIndexValue` strips a `_prefix` suffix from the index name and calls `PrefixFromArgs`
  instead of `FromArgs`. So `txn.Get("person", "name_prefix", "jo")` is a prefix scan over the `name` index.

Built-in indexers (String/StringSlice/StringMap/Int/Uint/Bool/UUID/FieldSet/Conditional/Compound/CompoundMulti) use
reflection over struct fields and encode values so that radix byte-order matches natural order — this is what makes
`LowerBound`/`ReverseLowerBound` range scans work for numeric types.

### Watches (`watch.go`, `watch_few.go`, `watch-gen/`)

`*Watch` query variants return the radix watch channel alongside the result; callers accumulate them in a `WatchSet`
and block in `WatchCtx`. Up to `aFew` (32) channels are watched in a single generated `select` in `watch_few.go`;
beyond that, `watchMany` chunks them into groups of 32, one goroutine each.

**`watch_few.go` is generated — do not edit it by hand.** Change `aFew` or the template in `watch-gen/main.go` and
run `go generate ./...`. The value 32 was tuned against `BenchmarkWatch`.

### Change tracking (`changes.go`)

`txn.TrackChanges()` before mutating makes the txn record `Change{Table, Before, After}` entries. `txn.Changes()`
de-duplicates by `(table, primaryKey)` so each object appears once with its first `Before` and last `After`, preserving
mutation order; an insert-then-delete within one txn collapses to nothing rather than a `nil`/`nil` change.

## Tests

Tests live alongside sources in the same package. `txn_test.go:testDB`, `schema_test.go:testValidSchema`, and
`index_test.go:testObj`/`TestObject` are the shared fixtures — reuse them rather than building new schemas.
`integ_test.go` covers end-to-end flows, `isolation_test.go` covers MVCC/snapshot isolation guarantees.
