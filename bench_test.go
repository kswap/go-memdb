// Copyright IBM Corp. 2015, 2026
// SPDX-License-Identifier: MPL-2.0

package memdb

// Macro benchmarks driving the public API. These exist to establish a baseline
// for the current go-immutable-radix backend so that a candidate backend can be
// A/B'd against it. The comparison is done by building the identical benchmark
// source in two git worktrees and running benchstat over the two outputs; the
// tree library never appears below, only memdb's public API, which is what makes
// that work.
//
//	git worktree add ../memdb-baseline main
//	go test -run '^$' -bench . -benchmem -count=10 -timeout 60m
//
// Steady-state memory is deliberately not measured with testing.B; see
// TestFootprint at the bottom of this file.

import (
	"flag"
	"fmt"
	"math/rand"
	"runtime"
	"runtime/debug"
	"testing"
)

// benchObj is the row type for every benchmark here. A single shared instance is
// reused when measuring tree footprint so the payload contributes O(1) rather
// than O(n).
type benchObj struct {
	ID   string
	Str  string
	Num  uint64
	Grp  string
	Tags []string
}

// benchSeed keeps every generated dataset reproducible across runs and across
// worktrees, which matters because the two sides of an A/B must see identical
// keys.
const benchSeed = 0x5eed

// groupSize is how many rows share a Grp value, and therefore how many rows a
// prefix scan over the "grp" index returns. Selectivity is controlled by varying
// this rather than by varying the table size.
const defaultGroupSize = 10

// keyShape describes one family of index keys. The shapes are chosen to span the
// cases where published ART results disagree with each other: maximum-entropy
// UUIDs, deeply prefix-shared strings, dense sequential integers, and sparse
// random integers.
type keyShape struct {
	name string
	// idIndexer builds the unique primary index for this shape.
	idIndexer Indexer
	// gen fills in the fields the primary index reads from.
	gen func(o *benchObj, i int, r *rand.Rand)
}

var benchShapes = []keyShape{
	{
		// 16 raw bytes after hex-decoding, essentially no shared prefixes.
		// The case where the Go ART's own published numbers lose to iradix.
		name:      "uuid",
		idIndexer: &UUIDFieldIndex{Field: "ID"},
		gen: func(o *benchObj, i int, r *rand.Rand) {
			var b [16]byte
			r.Read(b[:])
			o.ID = fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
		},
	},
	{
		// Long shared prefix, low fanout, deep path compression. Consul's
		// service and node names look like this.
		name:      "prefixstr",
		idIndexer: &StringFieldIndex{Field: "ID"},
		gen: func(o *benchObj, i int, r *rand.Rand) {
			o.ID = fmt.Sprintf("service/web/instance/%08d", i)
		},
	},
	{
		// Mid-fanout natural-language-ish keys with no fixed structure.
		name:      "word",
		idIndexer: &StringFieldIndex{Field: "ID"},
		gen: func(o *benchObj, i int, r *rand.Rand) {
			o.ID = randWord(r, 6+r.Intn(9))
		},
	},
	{
		// Dense big-endian keys: the top of the tree is wide, which is where
		// ART's Node256 is supposed to win and where copy-on-write hurts most.
		name:      "sequint",
		idIndexer: &UintFieldIndex{Field: "Num"},
		gen: func(o *benchObj, i int, r *rand.Rand) {
			o.Num = uint64(i)
		},
	},
	{
		// Sparse 64-bit keys: wide near the root, then long sparse tails.
		name:      "randuint",
		idIndexer: &UintFieldIndex{Field: "Num"},
		gen: func(o *benchObj, i int, r *rand.Rand) {
			o.Num = r.Uint64()
		},
	},
	{
		// Concatenated self-delimiting parts: heavy prefix sharing by
		// construction, and Consul's most common compound shape.
		name: "compound",
		idIndexer: &CompoundIndex{Indexes: []Indexer{
			&StringFieldIndex{Field: "Grp"},
			&UUIDFieldIndex{Field: "ID"},
		}},
		gen: func(o *benchObj, i int, r *rand.Rand) {
			var b [16]byte
			r.Read(b[:])
			o.ID = fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
		},
	},
}

func randWord(r *rand.Rand, n int) string {
	const alpha = "abcdefghijklmnopqrstuvwxyz"
	b := make([]byte, n)
	for i := range b {
		b[i] = alpha[r.Intn(len(alpha))]
	}
	return string(b)
}

// benchSchema builds a single-table schema whose "id" index has the given shape.
// extra selects how many secondary indexes to maintain; memdb writes every index
// on every Insert, so this axis can dominate write cost and is held explicit.
func benchSchema(shape keyShape, extra int) *DBSchema {
	idx := map[string]*IndexSchema{
		"id": {Name: "id", Unique: true, Indexer: shape.idIndexer},
	}
	if extra >= 1 {
		// Non-unique: memdb appends the primary key, so lookups become prefix
		// scans. This is the index the selectivity benchmarks scan.
		idx["grp"] = &IndexSchema{Name: "grp", Unique: false,
			Indexer: &StringFieldIndex{Field: "Grp"}}
	}
	if extra >= 2 {
		idx["str"] = &IndexSchema{Name: "str", Unique: false, AllowMissing: true,
			Indexer: &StringFieldIndex{Field: "Str"}}
	}
	if extra >= 3 {
		idx["num"] = &IndexSchema{Name: "num", Unique: false,
			Indexer: &UintFieldIndex{Field: "Num"}}
	}
	if extra >= 4 {
		// A MultiIndexer: one object yields several keys, amplifying write cost.
		idx["tags"] = &IndexSchema{Name: "tags", Unique: false, AllowMissing: true,
			Indexer: &StringSliceFieldIndex{Field: "Tags"}}
	}
	return &DBSchema{Tables: map[string]*TableSchema{
		"bench": {Name: "bench", Indexes: idx},
	}}
}

// benchRows generates n rows deterministically for the given shape.
func benchRows(shape keyShape, n, groupSize int) []*benchObj {
	r := rand.New(rand.NewSource(benchSeed))
	out := make([]*benchObj, n)
	for i := 0; i < n; i++ {
		o := &benchObj{
			Num:  uint64(i),
			Grp:  fmt.Sprintf("g%08d", i/groupSize),
			Str:  fmt.Sprintf("attr-%06d", i%1000),
			Tags: []string{"alpha", fmt.Sprintf("t%03d", i%97)},
		}
		shape.gen(o, i, r)
		out[i] = o
	}
	return out
}

// benchDB builds a populated database. The rows are returned so benchmarks can
// look up keys that are known to exist.
func benchDB(tb testing.TB, shape keyShape, n, extra int) (*MemDB, []*benchObj) {
	tb.Helper()
	db, err := NewMemDB(benchSchema(shape, extra))
	if err != nil {
		tb.Fatalf("schema: %v", err)
	}
	rows := benchRows(shape, n, defaultGroupSize)
	txn := db.Txn(true)
	for _, o := range rows {
		if err := txn.Insert("bench", o); err != nil {
			tb.Fatalf("insert: %v", err)
		}
	}
	txn.Commit()
	return db, rows
}

// benchSizes is the table-size sweep. The low end is not decoration: PostgreSQL
// found their radix tree used *more* memory than the sorted array it replaced
// for small sparse key sets, and a small high-cardinality secondary index is
// exactly that shape.
var benchSizes = []int{100, 10000, 1000000}

func sizeLabel(n int) string {
	switch {
	case n >= 1000000:
		return fmt.Sprintf("%dM", n/1000000)
	case n >= 1000:
		return fmt.Sprintf("%dk", n/1000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// ---------------------------------------------------------------------------
// Read path
// ---------------------------------------------------------------------------

// BenchmarkFirstHit measures an exact point lookup that finds its row. On a
// unique index this is the GetWatch fast path.
func BenchmarkFirstHit(b *testing.B) {
	for _, shape := range benchShapes {
		for _, n := range benchSizes {
			b.Run(shape.name+"/"+sizeLabel(n), func(b *testing.B) {
				db, rows := benchDB(b, shape, n, 1)
				txn := db.Txn(false)
				defer txn.Abort()
				args := lookupArgs(shape, rows)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					raw, err := txn.First("bench", "id", args[i%len(args)]...)
					if err != nil || raw == nil {
						b.Fatalf("miss: %v", err)
					}
				}
			})
		}
	}
}

// BenchmarkFirstMiss measures a lookup that finds nothing. This is the path that
// must still return a usable watch channel, so it exercises GetWatch-on-miss
// rather than short-circuiting.
func BenchmarkFirstMiss(b *testing.B) {
	for _, shape := range benchShapes {
		n := 10000
		b.Run(shape.name+"/"+sizeLabel(n), func(b *testing.B) {
			// Generate n+256 rows but insert only the first n. The withheld
			// tail is absent from the table while still being well-formed for
			// this shape's indexer, which a synthesised key would not be.
			const spare = 256
			db, err := NewMemDB(benchSchema(shape, 1))
			if err != nil {
				b.Fatal(err)
			}
			all := benchRows(shape, n+spare, defaultGroupSize)
			txn := db.Txn(true)
			for _, o := range all[:n] {
				if err := txn.Insert("bench", o); err != nil {
					b.Fatal(err)
				}
			}
			txn.Commit()
			missing := all[n:]

			txn = db.Txn(false)
			defer txn.Abort()
			args := lookupArgs(shape, missing)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := txn.First("bench", "id", args[i%len(args)]...); err != nil {
					b.Fatalf("err: %v", err)
				}
			}
		})
	}
}

// lookupArgs builds the FromArgs arguments matching a shape's primary index.
func lookupArgs(shape keyShape, rows []*benchObj) [][]interface{} {
	out := make([][]interface{}, 0, len(rows))
	for _, o := range rows {
		switch shape.name {
		case "sequint", "randuint":
			out = append(out, []interface{}{o.Num})
		case "compound":
			out = append(out, []interface{}{o.Grp, o.ID})
		default:
			out = append(out, []interface{}{o.ID})
		}
	}
	return out
}

// BenchmarkPrefixScan drains a non-unique index lookup. This is memdb's dominant
// read: Consul's state store makes 138 of these against 0 range seeks. Draining
// matters — seek-only benchmarks hide the iteration cost that published ART
// results say is its weakest point.
func BenchmarkPrefixScan(b *testing.B) {
	const n = 100000
	for _, shape := range benchShapes {
		for _, sel := range []int{1, 10, 1000} {
			b.Run(fmt.Sprintf("%s/rows=%d", shape.name, sel), func(b *testing.B) {
				db, err := NewMemDB(benchSchema(shape, 1))
				if err != nil {
					b.Fatal(err)
				}
				rows := benchRows(shape, n, sel)
				txn := db.Txn(true)
				for _, o := range rows {
					if err := txn.Insert("bench", o); err != nil {
						b.Fatal(err)
					}
				}
				txn.Commit()

				rt := db.Txn(false)
				defer rt.Abort()
				b.ReportAllocs()
				b.ResetTimer()
				count := 0
				for i := 0; i < b.N; i++ {
					grp := rows[(i*sel)%n].Grp
					it, err := rt.Get("bench", "grp", grp)
					if err != nil {
						b.Fatal(err)
					}
					for obj := it.Next(); obj != nil; obj = it.Next() {
						count++
					}
				}
				b.ReportMetric(float64(count)/float64(b.N), "rows/op")
			})
		}
	}
}

// BenchmarkFullScan drains the whole table through the primary index. Reported
// per row so the number is comparable across table sizes.
func BenchmarkFullScan(b *testing.B) {
	for _, shape := range benchShapes {
		for _, n := range []int{10000, 1000000} {
			b.Run(shape.name+"/"+sizeLabel(n), func(b *testing.B) {
				db, _ := benchDB(b, shape, n, 1)
				txn := db.Txn(false)
				defer txn.Abort()
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					it, err := txn.Get("bench", "id")
					if err != nil {
						b.Fatal(err)
					}
					seen := 0
					for obj := it.Next(); obj != nil; obj = it.Next() {
						seen++
					}
					if seen != n {
						b.Fatalf("scanned %d want %d", seen, n)
					}
				}
				b.ReportMetric(float64(n), "rows/op")
			})
		}
	}
}

// BenchmarkReverseScan exercises the reverse iterator, which in the current
// backend pays a map insert and delete per internal node visited.
func BenchmarkReverseScan(b *testing.B) {
	const n = 10000
	for _, shape := range benchShapes {
		b.Run(shape.name, func(b *testing.B) {
			db, _ := benchDB(b, shape, n, 1)
			txn := db.Txn(false)
			defer txn.Abort()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				it, err := txn.GetReverse("bench", "id")
				if err != nil {
					b.Fatal(err)
				}
				for obj := it.Next(); obj != nil; obj = it.Next() {
				}
			}
		})
	}
}

// BenchmarkLowerBound covers the range-seek API. Consul never calls it, but it
// is part of the contract and a candidate backend must not regress it.
func BenchmarkLowerBound(b *testing.B) {
	const n = 100000
	for _, shape := range []keyShape{benchShapes[3], benchShapes[4]} { // sequint, randuint
		for _, take := range []int{10, 1000} {
			b.Run(fmt.Sprintf("%s/take=%d", shape.name, take), func(b *testing.B) {
				db, rows := benchDB(b, shape, n, 1)
				txn := db.Txn(false)
				defer txn.Abort()
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					it, err := txn.LowerBound("bench", "id", rows[i%n].Num)
					if err != nil {
						b.Fatal(err)
					}
					for j := 0; j < take; j++ {
						if it.Next() == nil {
							break
						}
					}
				}
			})
		}
	}
}

// BenchmarkReverseLowerBound is the mirror of the above. It is called out
// separately because SeekReverseLowerBound is the most intricate function in the
// backend and the likeliest place for a replacement to be subtly wrong.
func BenchmarkReverseLowerBound(b *testing.B) {
	const n = 100000
	shape := benchShapes[3] // sequint
	db, rows := benchDB(b, shape, n, 1)
	txn := db.Txn(false)
	defer txn.Abort()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		it, err := txn.ReverseLowerBound("bench", "id", rows[i%n].Num)
		if err != nil {
			b.Fatal(err)
		}
		for j := 0; j < 10; j++ {
			if it.Next() == nil {
				break
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Write path
// ---------------------------------------------------------------------------

// BenchmarkInsertBatch is the most important write benchmark. Path copying is
// amortized within a transaction, so per-insert numbers measure a usage pattern
// nobody actually has. The batch sweep is what exposes that amortization, and it
// is the axis the abandoned ART branch's deleted benchmarks got wrong.
func BenchmarkInsertBatch(b *testing.B) {
	for _, shape := range benchShapes {
		for _, batch := range []int{1, 10, 100, 1000, 10000} {
			b.Run(fmt.Sprintf("%s/batch=%d", shape.name, batch), func(b *testing.B) {
				rows := benchRows(shape, batch, defaultGroupSize)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					b.StopTimer()
					db, err := NewMemDB(benchSchema(shape, 1))
					if err != nil {
						b.Fatal(err)
					}
					b.StartTimer()

					txn := db.Txn(true)
					for _, o := range rows {
						if err := txn.Insert("bench", o); err != nil {
							b.Fatal(err)
						}
					}
					txn.Commit()
				}
				b.ReportMetric(float64(batch), "rows/op")
			})
		}
	}
}

// BenchmarkInsertUpdate overwrites rows that already exist. memdb implements an
// update as a delete plus an insert in every index, so this is a different and
// heavier path than a fresh insert — and it is the operation the third-party Go
// ART's own benchmarks report losing on.
func BenchmarkInsertUpdate(b *testing.B) {
	const n = 100000
	for _, shape := range benchShapes {
		for _, extra := range []int{1, 4} {
			b.Run(fmt.Sprintf("%s/indexes=%d", shape.name, extra+1), func(b *testing.B) {
				db, rows := benchDB(b, shape, n, extra)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					o := *rows[i%n] // copy: never mutate an inserted object
					o.Str = fmt.Sprintf("updated-%d", i)
					txn := db.Txn(true)
					if err := txn.Insert("bench", &o); err != nil {
						b.Fatal(err)
					}
					txn.Commit()
				}
			})
		}
	}
}

// BenchmarkCommit isolates commit cost — the sub-transaction commits, the root
// pointer swap, and the notify pass — from the inserts that preceded it.
func BenchmarkCommit(b *testing.B) {
	shape := benchShapes[0] // uuid
	for _, batch := range []int{1, 100, 10000} {
		b.Run(fmt.Sprintf("batch=%d", batch), func(b *testing.B) {
			rows := benchRows(shape, batch, defaultGroupSize)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				db, err := NewMemDB(benchSchema(shape, 1))
				if err != nil {
					b.Fatal(err)
				}
				txn := db.Txn(true)
				for _, o := range rows {
					if err := txn.Insert("bench", o); err != nil {
						b.Fatal(err)
					}
				}
				b.StartTimer()

				txn.Commit()
			}
		})
	}
}

// BenchmarkDelete removes and reinserts a row so the table size stays fixed.
func BenchmarkDelete(b *testing.B) {
	const n = 100000
	for _, shape := range benchShapes {
		b.Run(shape.name, func(b *testing.B) {
			db, rows := benchDB(b, shape, n, 1)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				o := rows[i%n]
				txn := db.Txn(true)
				if err := txn.Delete("bench", o); err != nil {
					b.Fatal(err)
				}
				if err := txn.Insert("bench", o); err != nil {
					b.Fatal(err)
				}
				txn.Commit()
			}
		})
	}
}

// BenchmarkSnapshot measures the cost of taking a point-in-time snapshot, which
// should be a single atomic pointer load regardless of table size.
func BenchmarkSnapshot(b *testing.B) {
	db, _ := benchDB(b, benchShapes[0], 100000, 1)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = db.Snapshot()
	}
}

// ---------------------------------------------------------------------------
// Watches
// ---------------------------------------------------------------------------

// BenchmarkWriteNoWatch writes against a snapshot, where mutation tracking is
// disabled. Comparing it to BenchmarkWriteWithWatch isolates the cost of the
// watch machinery — which, in the current backend, is expected to be nearly zero
// because the channels are allocated whether or not tracking is on.
func BenchmarkWriteNoWatch(b *testing.B) {
	const n = 10000
	shape := benchShapes[0]
	db, rows := benchDB(b, shape, n, 1)
	snap := db.Snapshot()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		o := *rows[i%n]
		o.Str = "x"
		txn := snap.Txn(true)
		if err := txn.Insert("bench", &o); err != nil {
			b.Fatal(err)
		}
		txn.Commit()
	}
}

// BenchmarkWriteWithWatch is the same write against the primary, where every
// touched node's channel is tracked and closed at commit.
func BenchmarkWriteWithWatch(b *testing.B) {
	const n = 10000
	shape := benchShapes[0]
	db, rows := benchDB(b, shape, n, 1)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		o := *rows[i%n]
		o.Str = "x"
		txn := db.Txn(true)
		if err := txn.Insert("bench", &o); err != nil {
			b.Fatal(err)
		}
		txn.Commit()
	}
}

// BenchmarkNotifyCliff drives a single transaction past the backend's 8192
// tracked-channel limit, at which point it discards the tracking set and falls
// back to a full old-versus-new tree walk that allocates two strings per node
// visited. This is a cliff rather than a slope and it sits on the path of large
// snapshot restores. Nobody has measured it before.
// The transaction must mutate an *existing* tree, not build one from empty.
// Channels are only tracked in writeNode, which runs when an already-published
// node is copied; a tree built from scratch creates fresh nodes and tracks almost
// nothing, so a build-from-empty benchmark never reaches the fallback no matter
// how many rows it inserts.
func BenchmarkNotifyCliff(b *testing.B) {
	const base = 200000
	shape := benchShapes[0]
	// 4000 stays under the 8192 tracked-channel limit; 40000 should exceed it
	// and switch Notify over to the tree-walking fallback.
	for _, touched := range []int{100, 1000, 2000, 4000, 40000} {
		b.Run(fmt.Sprintf("touched=%d", touched), func(b *testing.B) {
			db, rows := benchDB(b, shape, base, 1)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				// Must be the primary: a snapshot has mutation tracking off, so
				// its Notify is a no-op and nothing would be measured. These are
				// updates rather than inserts, so the tree stays at `base` rows
				// and every iteration sees the same shape.
				txn := db.Txn(true)
				for j := 0; j < touched; j++ {
					o := *rows[j]
					o.Str = fmt.Sprintf("v%d", i)
					if err := txn.Insert("bench", &o); err != nil {
						b.Fatal(err)
					}
				}
				b.StartTimer()

				txn.Commit() // includes the notify pass
			}
			b.ReportMetric(float64(touched), "touched/op")
		})
	}
}

// BenchmarkSpuriousWakeups measures how many watchers fire on writes that touch
// none of the keys they watch.
//
// This is a correctness-adjacent metric, not a speed one, and it is the number
// most likely to disqualify a candidate backend. Coarser nodes mean a watcher
// registered on one key wakes for changes to unrelated keys that happen to share
// an ancestor. Nothing in the existing suite catches this: TestWatchUpdate
// asserts that correlated watches *do* fire and that snapshot writes do not, but
// never that an unrelated write stays quiet. In production a regression here
// means thundering-herd blocking-query wakeups across a Consul cluster.
//
// Lower is better. A perfect backend reports 0.
func BenchmarkSpuriousWakeups(b *testing.B) {
	const (
		n       = 100000
		watched = 512
	)
	for _, shape := range benchShapes {
		b.Run(shape.name, func(b *testing.B) {
			db, rows := benchDB(b, shape, n, 1)

			// Watch the first `watched` rows.
			txn := db.Txn(false)
			args := lookupArgs(shape, rows[:watched])
			chans := make([]<-chan struct{}, watched)
			for i := range chans {
				ch, _, err := txn.FirstWatch("bench", "id", args[i]...)
				if err != nil {
					b.Fatal(err)
				}
				chans[i] = ch
			}
			txn.Abort()

			// Write only to rows far outside the watched set.
			writes := 0
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				o := *rows[watched+(i%(n-watched))]
				o.Str = fmt.Sprintf("v%d", i)
				wt := db.Txn(true)
				if err := wt.Insert("bench", &o); err != nil {
					b.Fatal(err)
				}
				wt.Commit()
				writes++
			}
			b.StopTimer()

			fired := 0
			for _, ch := range chans {
				select {
				case <-ch:
					fired++
				default:
				}
			}
			if writes > 0 {
				b.ReportMetric(float64(fired)/float64(writes), "falseWake/op")
			}
			b.ReportMetric(float64(fired), "falseWake/total")
		})
	}
}

// ---------------------------------------------------------------------------
// Writer contention
// ---------------------------------------------------------------------------

// BenchmarkWriterContention measures parallel writers. memdb serializes every
// write in the whole database behind a single mutex, so this is expected to
// flatten as P rises regardless of which tree sits underneath.
//
// It is here so that a write-throughput problem is not misattributed to the
// radix tree. Run with -cpu=1,2,4,8 and, to see the lock directly:
//
//	go test -run '^$' -bench WriterContention -mutexprofile mu.out
//	go tool pprof -top mu.out
func BenchmarkWriterContention(b *testing.B) {
	const n = 10000
	shape := benchShapes[0]
	db, rows := benchDB(b, shape, n, 1)

	old := runtime.SetMutexProfileFraction(1)
	defer runtime.SetMutexProfileFraction(old)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			o := *rows[i%n]
			o.Str = "p"
			i++
			txn := db.Txn(true)
			if err := txn.Insert("bench", &o); err != nil {
				b.Fatal(err)
			}
			txn.Commit()
		}
	})
}

// BenchmarkParallelRead is the counterpart: readers take no lock at all, so this
// should scale close to linearly. If it does not, the problem is not the tree.
func BenchmarkParallelRead(b *testing.B) {
	const n = 100000
	shape := benchShapes[0]
	db, rows := benchDB(b, shape, n, 1)
	args := lookupArgs(shape, rows)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			txn := db.Txn(false)
			if _, err := txn.First("bench", "id", args[i%len(args)]...); err != nil {
				b.Fatal(err)
			}
			txn.Abort()
			i++
		}
	})
}

// ---------------------------------------------------------------------------
// Steady-state footprint
// ---------------------------------------------------------------------------

// TestFootprint reports steady-state memory per stored key.
//
// This deliberately is not a testing.B benchmark: footprint is not a
// per-iteration quantity, and b.N scaling corrupts it. It is skipped unless
// asked for:
//
//	go test -run TestFootprint -v -timeout 30m -footprint
//
// Three numbers are reported, not one. bytes/key is the figure the ART paper's
// whole argument rests on. objects/key is the diagnostic: the current backend
// spreads one leaf entry across four to five heap objects, two of which are
// watch channels, so a backend that allocates channels lazily should drop this
// by about two immediately. gcFraction catches the cost that no published ART
// benchmark measures — a tree of millions of pointers is a tree the Go collector
// must scan on every cycle.
func TestFootprint(t *testing.T) {
	if !footprintEnabled() {
		t.Skip("pass -footprint to run; this builds multi-million-row tables")
	}
	// Sweeping the index count isolates the per-index-entry cost, since a table
	// with one index pays it once per row and a table with two pays it twice.
	// That per-entry figure is what a lazily-allocated watch channel would cut
	// into, so it is the number that predicts whether the cheap fix is enough.
	for _, shape := range benchShapes {
		for _, extra := range []int{0, 1} {
			for _, n := range []int{100000, 1000000} {
				name := fmt.Sprintf("%s/idx=%d/%s", shape.name, extra+1, sizeLabel(n))
				t.Run(name, func(t *testing.T) {
					b, o, gc := measureFootprint(t, shape, n, extra)
					t.Logf("%-26s bytes/key=%8.1f  objects/key=%6.2f  bytes/entry=%8.1f  objects/entry=%5.2f  gcFraction=%.4f",
						name, b, o, b/float64(extra+1), o/float64(extra+1), gc)
				})
			}
		}
	}
}

func measureFootprint(tb testing.TB, shape keyShape, n, extra int) (bytesPerKey, objsPerKey, gcFraction float64) {
	// Stop the collector from moving the floor underneath us mid-build.
	prev := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(prev)

	rows := benchRows(shape, n, defaultGroupSize)

	var before, after runtime.MemStats
	// Twice: the first collection can leave finalizer-reachable garbage.
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&before)
	gcBefore := before.GCCPUFraction

	db, err := NewMemDB(benchSchema(shape, extra))
	if err != nil {
		tb.Fatal(err)
	}
	txn := db.Txn(true)
	for _, o := range rows {
		if err := txn.Insert("bench", o); err != nil {
			tb.Fatal(err)
		}
	}
	txn.Commit()
	txn = nil

	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&after)

	// Without this the compiler is free to collect the database before the
	// measurement above is taken.
	runtime.KeepAlive(db)
	runtime.KeepAlive(rows)

	bytesPerKey = float64(after.HeapAlloc-before.HeapAlloc) / float64(n)
	objsPerKey = float64(after.HeapObjects-before.HeapObjects) / float64(n)
	gcFraction = after.GCCPUFraction - gcBefore
	return
}

// footprintFlag gates TestFootprint. It is a registered flag rather than an
// os.Args scan so that the testing package's flag parser accepts it.
var footprintFlag = flag.Bool("footprint", false,
	"run the steady-state footprint measurement (builds multi-million-row tables)")

func footprintEnabled() bool { return *footprintFlag }
