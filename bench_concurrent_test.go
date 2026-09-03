// Copyright IBM Corp. 2015, 2026
// SPDX-License-Identifier: MPL-2.0

package memdb

import (
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// Concurrent benchmarks. Every other number in this package is single-goroutine,
// which leaves unmeasured the mechanism carrying the largest share of the memory
// win: with lazily allocated watch channels a reader materialises the channel on
// a published node by compare-and-swap. That is a write performed by readers, on
// shared memory, contending exactly when several readers ask the same node for a
// channel at once -- what a Consul server does when many blocking queries watch
// one popular service.
//
// How the writer is scheduled decides whether these numbers mean anything.
//
// A writer looping as fast as it can is not a control: a backend with faster
// writes then subjects its own readers to more commits, more garbage and more
// cache invalidation, so the reader comparison silently becomes a measurement of
// write throughput and the fastest backend looks worst. Rate-limiting with a
// timer does not fix it either -- at these periods Go's ticker coalesces and
// drops ticks under load, and the achieved rate lands far below target and
// varies by arm.
//
// So the writers are a fixed slice of the goroutines instead. Every arm gets
// GOMAXPROCS goroutines with the same number writing, which makes CPU allocation
// identical across arms and involves no timers. A faster backend then completes
// more writes from the same share, which is a real advantage rather than an
// artefact -- and because both rates are reported, a reader result can always be
// read against the write volume that produced it.
//
// Sharing shapes, to isolate the CAS path:
//
//	SameNode      every reader materialises one node's channel: worst case
//	DistinctNodes readers spread across services: the control
//	NoWatch       identical traffic that never asks for a channel: the floor
//
// If lazy allocation inverts under contention, it shows up as SameNode
// degrading against DistinctNodes within an arm.

const concurrentBenchRows = 200000

// writerGoroutines is how many of the GOMAXPROCS parallel goroutines commit
// instead of reading. 0 is a quiescent tree.
var writerGoroutines = []int{0, 1, 2}

// runParallelMixed runs GOMAXPROCS goroutines, nWriters of which commit
// single-row updates while the rest run read. Reads and writes are counted
// separately and reported as rates, because b.N is shared across both roles and
// so ns/op alone would describe a blend rather than either one.
func runParallelMixed(b *testing.B, nWriters int, read func(txn *Txn, i int) error) {
	rows := benchRows(concurrentBenchRows)
	db := benchDB(b, rows)

	var gid, reads, writes atomic.Int64
	b.ReportAllocs()
	b.ResetTimer()
	start := time.Now()
	b.RunParallel(func(pb *testing.PB) {
		id := int(gid.Add(1) - 1)
		isWriter := id < nWriters
		i := id * 7919 // co-prime stride, so goroutines do not walk in lockstep
		for pb.Next() {
			if isWriter {
				row := rows[i%len(rows)]
				txn := db.Txn(true)
				// A fresh value each time, so the write is never elided as a
				// no-op and the path to the leaf is genuinely re-copied.
				if err := txn.Insert("svc", &benchRow{
					ID:      row.ID,
					Service: row.Service,
					Node:    row.Node,
					Index:   uint64(i),
				}); err != nil {
					txn.Abort()
					b.Error(err)
					return
				}
				txn.Commit()
				writes.Add(1)
			} else {
				txn := db.Txn(false)
				if err := read(txn, i); err != nil {
					txn.Abort()
					b.Error(err)
					return
				}
				txn.Abort()
				reads.Add(1)
			}
			i++
		}
	})
	b.StopTimer()
	elapsed := time.Since(start).Seconds()
	b.ReportMetric(float64(reads.Load())/elapsed/1e6, "Mreads/sec")
	if nWriters > 0 {
		b.ReportMetric(float64(writes.Load())/elapsed/1e3, "kwrites/sec")
	}
}

func benchParallel(b *testing.B, read func(txn *Txn, i int) error) {
	if runtime.GOMAXPROCS(0) < 4 {
		b.Skip("needs at least 4 procs to separate readers from writers")
	}
	for _, n := range writerGoroutines {
		b.Run(fmt.Sprintf("writers=%d", n), func(b *testing.B) {
			runParallelMixed(b, n, read)
		})
	}
}

func benchServiceNames() []string {
	s := make([]string, benchServices)
	for i := range s {
		s[i] = fmt.Sprintf("service-%03d", i)
	}
	return s
}

// BenchmarkParallelWatchSameNode is the contended case: every reader asks the
// same service index node for a watch channel. Maximum sharing on the CAS path.
func BenchmarkParallelWatchSameNode(b *testing.B) {
	svc := fmt.Sprintf("service-%03d", 0)
	benchParallel(b, func(txn *Txn, _ int) error {
		ch, _, err := txn.FirstWatch("svc", "service", svc)
		if err != nil {
			return err
		}
		if ch == nil {
			return fmt.Errorf("expected a watch channel")
		}
		return nil
	})
}

// BenchmarkParallelWatchDistinctNodes is the control for SameNode: same traffic,
// same writers, but readers spread across every service so channel
// materialisation rarely collides.
func BenchmarkParallelWatchDistinctNodes(b *testing.B) {
	services := benchServiceNames()
	benchParallel(b, func(txn *Txn, i int) error {
		ch, _, err := txn.FirstWatch("svc", "service", services[i%len(services)])
		if err != nil {
			return err
		}
		if ch == nil {
			return fmt.Errorf("expected a watch channel")
		}
		return nil
	})
}

// BenchmarkParallelReadNoWatch is the floor: identical read traffic through
// First, which never asks for a channel. The gap from here to the two above is
// what the watch mechanism costs under concurrency.
func BenchmarkParallelReadNoWatch(b *testing.B) {
	services := benchServiceNames()
	benchParallel(b, func(txn *Txn, i int) error {
		_, err := txn.First("svc", "service", services[i%len(services)])
		return err
	})
}

// BenchmarkParallelPrefixScan drives memdb's dominant read -- the prefix scan
// behind 138 call sites in Consul's state store -- from many goroutines. This is
// where a per-scan iterator allocation turns into GC pressure rather than just a
// wider allocation.
func BenchmarkParallelPrefixScan(b *testing.B) {
	nodes := make([]string, benchNodes)
	for i := range nodes {
		nodes[i] = fmt.Sprintf("node-%05d", i)
	}
	benchParallel(b, func(txn *Txn, i int) error {
		it, err := txn.Get("svc", "node_prefix", nodes[i%len(nodes)])
		if err != nil {
			return err
		}
		for obj := it.Next(); obj != nil; obj = it.Next() {
		}
		return nil
	})
}
