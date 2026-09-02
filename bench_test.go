// Copyright IBM Corp. 2015, 2026
// SPDX-License-Identifier: MPL-2.0

package memdb

import (
	"fmt"
	"runtime"
	"testing"
	"time"
)

// Benchmarks for the index tree as memdb actually drives it. They are
// backend-neutral: run them once plain and once with -tags memdb_art, then
// compare with benchstat.
//
//	go test -run '^$' -bench . -benchmem -count=8 ./... > iradix.txt
//	go test -tags memdb_art -run '^$' -bench . -benchmem -count=8 ./... > art.txt
//	benchstat iradix.txt art.txt
//
// The schema below mirrors Consul's services table, which is what makes these
// numbers worth anything: a unique compound id of node and service, two
// non-unique string indexes, and a uint64 raft index. memdb null-terminates
// every string key and appends the primary key to non-unique index keys, so the
// key shapes here are the ones a Consul server really stores.

type benchRow struct {
	ID      string // "<node>/<service>", unique
	Service string
	Node    string
	Index   uint64
}

func benchSchema() *DBSchema {
	return &DBSchema{
		Tables: map[string]*TableSchema{
			"svc": {
				Name: "svc",
				Indexes: map[string]*IndexSchema{
					"id": {
						Name:    "id",
						Unique:  true,
						Indexer: &StringFieldIndex{Field: "ID"},
					},
					"service": {
						Name:    "service",
						Indexer: &StringFieldIndex{Field: "Service"},
					},
					"node": {
						Name:    "node",
						Indexer: &StringFieldIndex{Field: "Node"},
					},
					"index": {
						Name:    "index",
						Indexer: &UintFieldIndex{Field: "Index"},
					},
				},
			},
		},
	}
}

const (
	benchServices = 500 // distinct service names; rows per service = rows/500
	benchNodes    = 2000
)

// benchRows builds n rows whose key distribution matches a catalog: a bounded
// set of service names spread across a bounded set of nodes, so the "service"
// index has deep fan-out and the "id" index is sparse.
func benchRows(n int) []*benchRow {
	rows := make([]*benchRow, n)
	for i := 0; i < n; i++ {
		node := fmt.Sprintf("node-%05d", i%benchNodes)
		svc := fmt.Sprintf("service-%03d", i%benchServices)
		rows[i] = &benchRow{
			ID:      node + "/" + svc + "/" + fmt.Sprintf("%06d", i),
			Service: svc,
			Node:    node,
			Index:   uint64(i),
		}
	}
	return rows
}

func benchDB(tb testing.TB, rows []*benchRow) *MemDB {
	db, err := NewMemDB(benchSchema())
	if err != nil {
		tb.Fatalf("err: %v", err)
	}
	txn := db.Txn(true)
	for _, row := range rows {
		if err := txn.Insert("svc", row); err != nil {
			tb.Fatalf("err: %v", err)
		}
	}
	txn.Commit()
	return db
}

// BenchmarkInsert is the raft-apply shape: one row, one transaction, one commit,
// against an already-populated table. Rows repeat once the pool is exhausted,
// at which point the insert becomes an in-place update — which is itself the
// common case in a Consul catalog, where agents re-register on a timer.
func BenchmarkInsert(b *testing.B) {
	const preload = 100000
	db := benchDB(b, benchRows(preload))
	pool := benchRows(preload)
	for i, row := range pool {
		row.ID = fmt.Sprintf("fresh-%06d/%s", i, row.Service)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		txn := db.Txn(true)
		if err := txn.Insert("svc", pool[i%len(pool)]); err != nil {
			b.Fatal(err)
		}
		txn.Commit()
	}
}

// BenchmarkBatchInsert is the snapshot-restore shape: many rows batched into one
// transaction, so the path-copy cost is amortised across the batch and the
// transaction's modified-node tracking has room to matter.
func BenchmarkBatchInsert(b *testing.B) {
	for _, batch := range []int{100, 1000} {
		b.Run(fmt.Sprintf("batch=%d", batch), func(b *testing.B) {
			rows := benchRows(batch)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				db, err := NewMemDB(benchSchema())
				if err != nil {
					b.Fatal(err)
				}
				txn := db.Txn(true)
				for _, row := range rows {
					if err := txn.Insert("svc", row); err != nil {
						b.Fatal(err)
					}
				}
				txn.Commit()
			}
		})
	}
}

// BenchmarkFirst is a point read on the unique index — the shape behind every
// "look this one thing up" path in a state store.
func BenchmarkFirst(b *testing.B) {
	rows := benchRows(100000)
	db := benchDB(b, rows)

	txn := db.Txn(false)
	defer txn.Abort()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		raw, err := txn.First("svc", "id", rows[i%len(rows)].ID)
		if err != nil {
			b.Fatal(err)
		}
		if raw == nil {
			b.Fatal("miss")
		}
	}
}

// BenchmarkFirstWatch is the same read on the blocking-query path, which pays
// for materialising a watch channel on every call. go-immutable-art allocates
// those lazily where go-immutable-radix allocates one per node up front, so the
// gap between this and BenchmarkFirst is the interesting number.
func BenchmarkFirstWatch(b *testing.B) {
	rows := benchRows(100000)
	db := benchDB(b, rows)

	txn := db.Txn(false)
	defer txn.Abort()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, raw, err := txn.FirstWatch("svc", "id", rows[i%len(rows)].ID)
		if err != nil {
			b.Fatal(err)
		}
		if raw == nil {
			b.Fatal("miss")
		}
	}
}

// BenchmarkGetPrefix walks every row under one service name — the shape behind
// a health or catalog query for a named service.
func BenchmarkGetPrefix(b *testing.B) {
	for _, total := range []int{10000, 200000} {
		b.Run(fmt.Sprintf("rows=%d", total), func(b *testing.B) {
			db := benchDB(b, benchRows(total))
			txn := db.Txn(false)
			defer txn.Abort()

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				it, err := txn.Get("svc", "service", fmt.Sprintf("service-%03d", i%benchServices))
				if err != nil {
					b.Fatal(err)
				}
				n := 0
				for raw := it.Next(); raw != nil; raw = it.Next() {
					n++
				}
				if n == 0 {
					b.Fatal("empty prefix scan")
				}
			}
		})
	}
}

// BenchmarkGetAll is a full ordered scan of the table — the shape behind
// ServiceDump and the usage-metrics sweep.
func BenchmarkGetAll(b *testing.B) {
	const total = 200000
	db := benchDB(b, benchRows(total))
	txn := db.Txn(false)
	defer txn.Abort()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		it, err := txn.Get("svc", "id")
		if err != nil {
			b.Fatal(err)
		}
		n := 0
		for raw := it.Next(); raw != nil; raw = it.Next() {
			n++
		}
		if n != total {
			b.Fatalf("walked %d of %d", n, total)
		}
	}
}

// BenchmarkLowerBound seeks into the uint64 raft-index and walks forward. This
// is the densest key shape in the schema — sequential integers pack a radix
// node completely, which is where a flat edge slice is at its best, so this is
// the benchmark most likely to show a regression rather than a win.
func BenchmarkLowerBound(b *testing.B) {
	const total = 200000
	db := benchDB(b, benchRows(total))
	txn := db.Txn(false)
	defer txn.Abort()

	for _, walk := range []int{10, 1000} {
		b.Run(fmt.Sprintf("walk=%d", walk), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				it, err := txn.LowerBound("svc", "index", uint64(i%(total/2)))
				if err != nil {
					b.Fatal(err)
				}
				for n := 0; n < walk; n++ {
					if raw := it.Next(); raw == nil {
						break
					}
				}
			}
		})
	}
}

// BenchmarkDelete removes a row from all four indexes and commits.
func BenchmarkDelete(b *testing.B) {
	const preload = 100000
	rows := benchRows(preload)

	b.ReportAllocs()
	b.ResetTimer()

	i := 0
	for i < b.N {
		b.StopTimer()
		db := benchDB(b, rows)
		b.StartTimer()

		for j := 0; j < preload && i < b.N; j, i = j+1, i+1 {
			txn := db.Txn(true)
			if err := txn.Delete("svc", rows[j]); err != nil {
				b.Fatal(err)
			}
			txn.Commit()
		}
	}
}

// BenchmarkWatchFire is the blocking-query wake path, and the one an operator
// feels most directly: a thousand watchers parked on distinct prefixes, then one
// write. It measures what a single commit costs when the tree is covered in
// live watch channels — tracking the modified nodes, publishing the new root,
// and closing every affected channel.
func BenchmarkWatchFire(b *testing.B) {
	for _, watchers := range []int{100, 1000} {
		b.Run(fmt.Sprintf("watchers=%d", watchers), func(b *testing.B) {
			rows := benchRows(100000)
			db := benchDB(b, rows)

			// Park watchers on distinct service prefixes. They are never woken
			// during the run — the write below touches one service only — so
			// what is measured is the cost of committing into a watched tree.
			txn := db.Txn(false)
			ws := NewWatchSet()
			for i := 0; i < watchers; i++ {
				it, err := txn.Get("svc", "service", fmt.Sprintf("service-%03d", i%benchServices))
				if err != nil {
					b.Fatal(err)
				}
				ws.Add(it.WatchCh())
			}
			txn.Abort()

			hot := &benchRow{
				ID:      "hot-node/hot-service/000000",
				Service: "hot-service",
				Node:    "hot-node",
				Index:   1,
			}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				wtxn := db.Txn(true)
				hot.Index = uint64(i)
				if err := wtxn.Insert("svc", hot); err != nil {
					b.Fatal(err)
				}
				wtxn.Commit()
			}
		})
	}
}

// BenchmarkWatchWake measures the other half: end-to-end latency from a commit
// that touches a watched prefix to the watcher actually waking.
func BenchmarkWatchWake(b *testing.B) {
	rows := benchRows(100000)
	db := benchDB(b, rows)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		txn := db.Txn(false)
		it, err := txn.Get("svc", "service", "service-042")
		if err != nil {
			b.Fatal(err)
		}
		ws := NewWatchSet()
		ws.Add(it.WatchCh())
		txn.Abort()

		wtxn := db.Txn(true)
		if err := wtxn.Insert("svc", &benchRow{
			ID:      fmt.Sprintf("waker/service-042/%06d", i),
			Service: "service-042",
			Node:    "waker",
			Index:   uint64(i),
		}); err != nil {
			b.Fatal(err)
		}
		wtxn.Commit()

		if timedOut := ws.Watch(time.After(5 * time.Second)); timedOut {
			b.Fatal("watcher never woke")
		}
	}
}

// BenchmarkSnapshot measures taking a point-in-time snapshot of the whole DB.
func BenchmarkSnapshot(b *testing.B) {
	db := benchDB(b, benchRows(200000))

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		snap := db.Snapshot()
		if snap == nil {
			b.Fatal("nil snapshot")
		}
	}
}

// BenchmarkHeap reports resident bytes per row for a fully-built table. This is
// the figure that translates most directly into a Consul server's RSS, so it is
// reported per row rather than left as a total.
func BenchmarkHeap(b *testing.B) {
	const total = 200000
	rows := benchRows(total)

	b.ReportAllocs()
	b.ResetTimer()

	var perRow float64
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)
		b.StartTimer()

		db := benchDB(b, rows)

		b.StopTimer()
		runtime.GC()
		var after runtime.MemStats
		runtime.ReadMemStats(&after)
		perRow = float64(after.HeapAlloc-before.HeapAlloc) / float64(total)
		runtime.KeepAlive(db)
		b.StartTimer()
	}
	b.ReportMetric(perRow, "B/row")
}
