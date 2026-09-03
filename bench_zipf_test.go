// Copyright IBM Corp. 2015, 2026
// SPDX-License-Identifier: MPL-2.0

package memdb

import (
	"fmt"
	"math/rand/v2"
	"runtime"
	"sort"
	"testing"
)

// A realistic catalog shape.
//
// benchRows spreads instances evenly: i%benchServices gives every service the
// same instance count, so every subtree of the service index has the same depth
// and the same node-kind mix, and a prefix scan always walks the same distance.
// Real catalogs do not look like that. A handful of services carry thousands of
// instances and a long tail carry one or two, which is what decides tree depth,
// the distribution of node kinds an adaptive tree selects, and how much work a
// scan does -- all three of the things this evaluation is trying to measure.
//
// Uniformity flatters both backends, but not equally: an adaptive radix tree
// picks its node kind from fan-out, so a fixture with one fan-out everywhere
// exercises one node kind and hides the adaptivity that is the whole point of
// the design. These fixtures restore the skew.
//
// The exponent is the knob. s just above 1 is the classic Zipf that shows up in
// service-mesh catalogs; larger s concentrates harder. THESE ARE STAND-INS --
// the trial plan calls for distributions taken from real fleet telemetry, and
// the exponent here should be replaced with a fitted one before any number
// derived from it is quoted as a prediction.
const (
	zipfExponent = 1.1 // s: skew. >1, closer to 1 is flatter
	zipfV        = 1.0 // v: Zipf offset, 1 means rank 0 is the most popular
	zipfSeed     = 0x5eed1e55
	zipfRowCount = 200000 // matches BenchmarkHeap, so the shapes are comparable
)

// zipfSweep is the range of skews the benchmarks run across.
//
// Consul ships to everyone, so no single exponent is the right one to fit: the
// question a vendor has to answer is not "does this win on our catalog" but
// "does it win across the catalogs customers actually run, and is there a shape
// where it regresses" -- because a shape that regresses is someone's production
// cluster. Sweeping turns the unknown into a reported surface instead of an
// assumption, and if the arm ordering holds across the whole range then no
// fleet measurement was ever needed to make the decision.
//
// s=0 is the uniform control -- the flat extreme, and the shape the original
// fixture had. Go's Zipf generator requires s > 1, so 1.05 stands in for the
// flat-but-skewed end; by 1.4 the head is heavily concentrated. Together they
// bracket what service catalogs plausibly look like, and the uniform control
// bounds the flat side that the generator itself cannot reach.
var zipfSweep = []struct {
	name string
	s    float64
}{
	{"uniform", 0},
	{"s=1.05", 1.05},
	{"s=1.1", 1.1},
	{"s=1.2", 1.2},
	{"s=1.4", 1.4},
}

// rowsForSkew returns the fixture for a given skew, with s<=0 meaning the
// uniform shape benchRows already builds.
func rowsForSkew(n int, s float64) []*benchRow {
	if s <= 0 {
		return benchRows(n)
	}
	return benchRowsZipf(n, s)
}

// benchRowsZipf builds n rows whose service popularity follows a Zipf
// distribution over benchServices names, with nodes spread uniformly. Seeded, so
// every arm and every round sees byte-identical input.
func benchRowsZipf(n int, s float64) []*benchRow {
	r := rand.New(rand.NewPCG(zipfSeed, zipfSeed>>3))
	z := rand.NewZipf(r, s, zipfV, uint64(benchServices-1))
	// NewZipf returns nil for s <= 1 rather than reporting it, and the nil only
	// surfaces as a panic inside Uint64 far from the cause.
	if z == nil {
		panic(fmt.Sprintf("benchRowsZipf: no Zipf generator for s=%v (requires s > 1)", s))
	}
	rows := make([]*benchRow, n)
	for i := 0; i < n; i++ {
		node := fmt.Sprintf("node-%05d", i%benchNodes)
		svc := fmt.Sprintf("service-%03d", z.Uint64())
		rows[i] = &benchRow{
			ID:      node + "/" + svc + "/" + fmt.Sprintf("%06d", i),
			Service: svc,
			Node:    node,
			Index:   uint64(i),
		}
	}
	return rows
}

// serviceSizes returns instance counts per service, descending.
func serviceSizes(rows []*benchRow) []int {
	byName := map[string]int{}
	for _, r := range rows {
		byName[r.Service]++
	}
	sizes := make([]int, 0, len(byName))
	for _, c := range byName {
		sizes = append(sizes, c)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(sizes)))
	return sizes
}

// TestCatalogShape prints both distributions so the fixture a number came from
// can be inspected rather than assumed. It asserts only that the skewed fixture
// is in fact skewed, which is what makes it worth having.
func TestCatalogShape(t *testing.T) {
	const n = 200000
	t.Logf("%-9s %9s %9s %9s %9s %9s", "shape", "services", "largest", "p50", "smallest", "top10%")
	for _, c := range zipfSweep {
		sizes := serviceSizes(rowsForSkew(n, c.s))
		total, top10 := 0, 0
		for i, v := range sizes {
			total += v
			if i < 10 {
				top10 += v
			}
		}
		t.Logf("%-9s %9d %9d %9d %9d %8.1f%%",
			c.name, len(sizes), sizes[0], sizes[len(sizes)/2], sizes[len(sizes)-1],
			100*float64(top10)/float64(total))
	}

	uniform := serviceSizes(benchRows(n))
	if uniform[0] != uniform[len(uniform)-1] {
		t.Errorf("uniform fixture is not uniform: largest=%d smallest=%d",
			uniform[0], uniform[len(uniform)-1])
	}
	// Each step up in s must concentrate the head further, or the sweep is not
	// actually sweeping anything.
	prev := 0
	for _, c := range zipfSweep {
		if c.s <= 0 {
			continue
		}
		got := serviceSizes(rowsForSkew(n, c.s))[0]
		if got <= prev {
			t.Errorf("s=%v largest service %d did not grow beyond previous %d", c.s, got, prev)
		}
		prev = got
	}
}

func zipfDB(tb testing.TB, rows []*benchRow) *MemDB { return benchDB(tb, rows) }

// BenchmarkZipfInsert is the raft-apply shape against a skewed catalog: most
// writes land in the popular subtrees, which is where node kinds are widest.
func BenchmarkZipfInsert(b *testing.B) {
	for _, c := range zipfSweep {
		b.Run(c.name, func(b *testing.B) {
			rows := rowsForSkew(zipfRowCount, c.s)
			db := zipfDB(b, rows)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				txn := db.Txn(true)
				if err := txn.Insert("svc", rows[i%len(rows)]); err != nil {
					b.Fatalf("err: %v", err)
				}
				txn.Commit()
			}
		})
	}
}

// BenchmarkZipfGetPrefix separates the head of the distribution from its tail.
// They are the same call against the same index; only fan-out differs, which is
// exactly what an adaptive tree is supposed to exploit and a uniform fixture
// cannot show.
func BenchmarkZipfGetPrefix(b *testing.B) {
	for _, c := range zipfSweep {
		rows := rowsForSkew(zipfRowCount, c.s)
		db := zipfDB(b, rows)

		counts := map[string]int{}
		for _, r := range rows {
			counts[r.Service]++
		}
		var names []string
		for n := range counts {
			names = append(names, n)
		}
		sort.Slice(names, func(i, j int) bool { return counts[names[i]] > counts[names[j]] })
		hot, cold := names[0], names[len(names)-1]

		for _, t := range []struct {
			label string
			svc   string
		}{{"hot", hot}, {"cold", cold}} {
			b.Run(c.name+"/"+t.label, func(b *testing.B) {
				b.ReportMetric(float64(counts[t.svc]), "instances")
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					txn := db.Txn(false)
					it, err := txn.Get("svc", "service", t.svc)
					if err != nil {
						b.Fatalf("err: %v", err)
					}
					for obj := it.Next(); obj != nil; obj = it.Next() {
					}
					txn.Abort()
				}
			})
		}
	}
}

// BenchmarkZipfHeap is steady-state bytes per index entry on the skewed shape,
// the counterpart to BenchmarkHeap on the uniform one. Comparing the two says
// whether the memory result depends on the fixture being uniform.
func BenchmarkZipfHeap(b *testing.B) {
	for _, c := range zipfSweep {
		b.Run(c.name, func(b *testing.B) { zipfHeapOne(b, c.s) })
	}
}

func zipfHeapOne(b *testing.B, skew float64) {
	const total = zipfRowCount
	rows := rowsForSkew(total, skew)

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
