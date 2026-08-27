// Copyright IBM Corp. 2015, 2026
// SPDX-License-Identifier: MPL-2.0

// Package bench compares the persistent radix tree go-memdb uses against a
// well-engineered persistent Adaptive Radix Tree, at the tree level.
//
// The question this answers is narrow and deliberate: would a *good* Go
// persistent ART beat go-memdb's improved radix tree on go-memdb's own key
// shapes? It is asked here rather than by porting memdb, because a port would
// confound the tree's behaviour with integration work, and because the only
// existing memdb-on-ART port is built on an implementation whose flaws run in
// both directions — Go-interface child pointers make its memory look worse than
// ART deserves, while a First that skips its watch channel makes its reads look
// better.
//
// cilium/statedb/part is used as the ART instead. It is the reference-quality
// persistent ART in Go: 16-byte node headers, raw pointers rather than
// interfaces, transaction-ID path copying, and watch channels created on demand
// — the same set of choices this repository's own tree now makes, so the
// comparison isolates node structure rather than bookkeeping.
//
// Both trees are driven directly. There is no interface shim in any measured
// path, because the indirection would bias the result.
//
//	go test -bench . -benchmem -count=6
//	go test -run TestTreeFootprint -v -footprint
package bench

import (
	"flag"
	"fmt"
	"math/rand"
	"runtime"
	"runtime/debug"
	"testing"

	"github.com/cilium/statedb/part"
	"github.com/hashicorp/go-memdb/internal/iradix"
)

// ---------------------------------------------------------------------------
// Key corpus
// ---------------------------------------------------------------------------

// keySet is one family of index keys, encoded exactly as go-memdb's indexers
// encode them: strings null-terminated, integers big-endian, UUIDs as 16 raw
// bytes. The shapes span the cases where published ART results disagree.
type keySet struct {
	name string
	keys [][]byte
}

const corpusSeed = 0x5eed

func buildCorpus(n int) []keySet {
	r := rand.New(rand.NewSource(corpusSeed))

	uuid := make([][]byte, n)
	for i := range uuid {
		b := make([]byte, 16)
		r.Read(b)
		uuid[i] = b
	}

	prefixstr := make([][]byte, n)
	for i := range prefixstr {
		prefixstr[i] = []byte(fmt.Sprintf("service/web/instance/%08d\x00", i))
	}

	word := make([][]byte, n)
	for i := range word {
		const alpha = "abcdefghijklmnopqrstuvwxyz"
		l := 6 + r.Intn(9)
		b := make([]byte, l+1)
		for j := 0; j < l; j++ {
			b[j] = alpha[r.Intn(len(alpha))]
		}
		word[i] = b
	}

	sequint := make([][]byte, n)
	for i := range sequint {
		sequint[i] = beUint64(uint64(i))
	}

	randuint := make([][]byte, n)
	for i := range randuint {
		randuint[i] = beUint64(r.Uint64())
	}

	// Group prefix plus a UUID, the shape Consul's compound indexes take.
	compound := make([][]byte, n)
	for i := range compound {
		b := make([]byte, 0, 26)
		b = append(b, fmt.Sprintf("g%08d\x00", i/10)...)
		u := make([]byte, 16)
		r.Read(u)
		compound[i] = append(b, u...)
	}

	return []keySet{
		{"uuid", uuid},
		{"prefixstr", prefixstr},
		{"word", word},
		{"sequint", sequint},
		{"randuint", randuint},
		{"compound", compound},
	}
}

func beUint64(v uint64) []byte {
	return []byte{
		byte(v >> 56), byte(v >> 48), byte(v >> 40), byte(v >> 32),
		byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v),
	}
}

// corpus is built once; every benchmark sees identical keys so the two trees
// are never compared on different data.
var corpus = buildCorpus(corpusSize)

const corpusSize = 200000

// value is what both trees store. go-memdb stores interface{}, so part is
// instantiated at `any` rather than a concrete type: unboxing would be a real
// advantage of generics, but not one available to memdb as it stands, and
// crediting it here would flatter the ART for the wrong reason.
var value any = struct{ x int }{42}

// ---------------------------------------------------------------------------
// Tree construction
// ---------------------------------------------------------------------------

func buildRadix(keys [][]byte) *iradix.Tree {
	tree := iradix.New()
	txn := tree.Txn()
	for _, k := range keys {
		txn.Insert(k, value)
	}
	return txn.CommitOnly()
}

func buildART(keys [][]byte) part.Tree[any] {
	tree := part.New[any]()
	txn := tree.Txn()
	for _, k := range keys {
		txn.Insert(k, value)
	}
	return txn.Commit()
}

// ---------------------------------------------------------------------------
// Point lookups
// ---------------------------------------------------------------------------

func BenchmarkGetHit(b *testing.B) {
	for _, ks := range corpus {
		b.Run("radix/"+ks.name, func(b *testing.B) {
			tree := buildRadix(ks.keys)
			root := tree.Root()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, ok := root.Get(ks.keys[i%len(ks.keys)]); !ok {
					b.Fatal("miss")
				}
			}
		})
		b.Run("art/"+ks.name, func(b *testing.B) {
			tree := buildART(ks.keys)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, ok := tree.Get(ks.keys[i%len(ks.keys)]); !ok {
					b.Fatal("miss")
				}
			}
		})
	}
}

// BenchmarkGetMiss looks up keys that are absent. Radix trees terminate early
// on a prefix mismatch, so an unsuccessful lookup is not simply a successful
// one without the final compare.
func BenchmarkGetMiss(b *testing.B) {
	for _, ks := range corpus {
		absent := absentKeys(ks)
		b.Run("radix/"+ks.name, func(b *testing.B) {
			tree := buildRadix(ks.keys)
			root := tree.Root()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				root.Get(absent[i%len(absent)])
			}
		})
		b.Run("art/"+ks.name, func(b *testing.B) {
			tree := buildART(ks.keys)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				tree.Get(absent[i%len(absent)])
			}
		})
	}
}

// absentKeys derives keys of the right shape that are not in the set, by
// flipping the leading byte of a sample. Synthesising differently-shaped keys
// would measure prefix mismatch at depth zero rather than a realistic miss.
func absentKeys(ks keySet) [][]byte {
	out := make([][]byte, 0, 256)
	for i := 0; i < 256 && i < len(ks.keys); i++ {
		k := append([]byte(nil), ks.keys[i]...)
		k[0] ^= 0xff
		out = append(out, k)
	}
	return out
}

// ---------------------------------------------------------------------------
// Writes
// ---------------------------------------------------------------------------

// BenchmarkInsertBatch sweeps how many keys are written per transaction. This
// is the axis that matters for a persistent tree: path copying is amortized
// within a transaction, so a per-insert figure measures a usage pattern nobody
// has, and the amortization is exactly what makes an ART's wide nodes bearable
// under copy-on-write.
func BenchmarkInsertBatch(b *testing.B) {
	for _, ks := range corpus {
		if ks.name != "uuid" && ks.name != "sequint" {
			continue // two representative shapes; the sweep is already large
		}
		for _, batch := range []int{1, 100, 10000} {
			keys := ks.keys[:batch]
			b.Run(fmt.Sprintf("radix/%s/batch=%d", ks.name, batch), func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					txn := iradix.New().Txn()
					for _, k := range keys {
						txn.Insert(k, value)
					}
					txn.CommitOnly()
				}
				b.ReportMetric(float64(batch), "keys/op")
			})
			b.Run(fmt.Sprintf("art/%s/batch=%d", ks.name, batch), func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					tree := part.New[any]()
					txn := tree.Txn()
					for _, k := range keys {
						txn.Insert(k, value)
					}
					txn.Commit()
				}
				b.ReportMetric(float64(batch), "keys/op")
			})
		}
	}
}

// BenchmarkUpdate overwrites keys that already exist, one transaction each.
// This is go-memdb's common write: an Insert of an existing object is a delete
// plus an insert in every index. Published ART numbers report losing here.
func BenchmarkUpdate(b *testing.B) {
	for _, ks := range corpus {
		b.Run("radix/"+ks.name, func(b *testing.B) {
			tree := buildRadix(ks.keys)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				txn := tree.Txn()
				txn.Insert(ks.keys[i%len(ks.keys)], value)
				tree = txn.CommitOnly()
			}
		})
		b.Run("art/"+ks.name, func(b *testing.B) {
			tree := buildART(ks.keys)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				txn := tree.Txn()
				txn.Insert(ks.keys[i%len(ks.keys)], value)
				tree = txn.Commit()
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Iteration — the operation that decides this
// ---------------------------------------------------------------------------

// BenchmarkFullIterate drains the whole tree.
//
// This is the most important benchmark here. go-memdb's dominant read is a
// prefix scan that gets drained to exhaustion — Consul's state store makes 138
// such calls against zero range seeks — and iteration is the operation every
// surveyed persistent-ART system is candid about being weak at, because walking
// a sparsely-populated wide node means stepping over empty slots.
func BenchmarkFullIterate(b *testing.B) {
	for _, ks := range corpus {
		b.Run("radix/"+ks.name, func(b *testing.B) {
			tree := buildRadix(ks.keys)
			root := tree.Root()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				it := root.Iterator()
				n := 0
				for _, _, ok := it.Next(); ok; _, _, ok = it.Next() {
					n++
				}
				if n != len(ks.keys) {
					b.Fatalf("walked %d want %d", n, len(ks.keys))
				}
			}
			b.ReportMetric(float64(len(ks.keys)), "keys/op")
		})
		b.Run("art/"+ks.name, func(b *testing.B) {
			tree := buildART(ks.keys)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				it := tree.Iterator()
				n := 0
				for _, _, ok := it.Next(); ok; _, _, ok = it.Next() {
					n++
				}
				if n != len(ks.keys) {
					b.Fatalf("walked %d want %d", n, len(ks.keys))
				}
			}
			b.ReportMetric(float64(len(ks.keys)), "keys/op")
		})
	}
}

// BenchmarkPrefixScan drains a narrow prefix range, the shape a non-unique
// index lookup takes in go-memdb once the primary key has been appended.
func BenchmarkPrefixScan(b *testing.B) {
	ks := corpus[5] // compound: "g%08d\x00" + uuid, ten keys per group
	prefixes := make([][]byte, 0, 256)
	for i := 0; i < 256; i++ {
		prefixes = append(prefixes, []byte(fmt.Sprintf("g%08d\x00", i)))
	}
	b.Run("radix", func(b *testing.B) {
		tree := buildRadix(ks.keys)
		root := tree.Root()
		b.ReportAllocs()
		b.ResetTimer()
		total := 0
		for i := 0; i < b.N; i++ {
			it := root.Iterator()
			it.SeekPrefix(prefixes[i%len(prefixes)])
			for _, _, ok := it.Next(); ok; _, _, ok = it.Next() {
				total++
			}
		}
		b.ReportMetric(float64(total)/float64(b.N), "keys/op")
	})
	// The pull form. This is the one memdb could actually use, because its
	// public ResultIterator contract is Next()-based.
	b.Run("art-next", func(b *testing.B) {
		tree := buildART(ks.keys)
		b.ReportAllocs()
		b.ResetTimer()
		total := 0
		for i := 0; i < b.N; i++ {
			it := tree.Prefix(prefixes[i%len(prefixes)])
			for _, _, ok := it.Next(); ok; _, _, ok = it.Next() {
				total++
			}
		}
		b.ReportMetric(float64(total)/float64(b.N), "keys/op")
	})
	// The push form, for fairness: All keeps its traversal stack on the Go
	// stack, where Next has to heap-allocate one because it must survive
	// between calls. memdb cannot consume this without inverting control of
	// every caller of Get, so it is shown as the ceiling rather than as an
	// option.
	b.Run("art-all", func(b *testing.B) {
		tree := buildART(ks.keys)
		b.ReportAllocs()
		b.ResetTimer()
		total := 0
		for i := 0; i < b.N; i++ {
			it := tree.Prefix(prefixes[i%len(prefixes)])
			it.All(func(_ []byte, _ any) bool {
				total++
				return true
			})
		}
		b.ReportMetric(float64(total)/float64(b.N), "keys/op")
	})
}

// BenchmarkLowerBound seeks then takes a bounded run.
func BenchmarkLowerBound(b *testing.B) {
	for _, name := range []string{"sequint", "randuint"} {
		ks := corpusByName(name)
		for _, take := range []int{10, 1000} {
			b.Run(fmt.Sprintf("radix/%s/take=%d", name, take), func(b *testing.B) {
				tree := buildRadix(ks.keys)
				root := tree.Root()
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					it := root.Iterator()
					it.SeekLowerBound(ks.keys[i%len(ks.keys)])
					for j := 0; j < take; j++ {
						if _, _, ok := it.Next(); !ok {
							break
						}
					}
				}
			})
			b.Run(fmt.Sprintf("art/%s/take=%d", name, take), func(b *testing.B) {
				tree := buildART(ks.keys)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					it := tree.LowerBound(ks.keys[i%len(ks.keys)])
					for j := 0; j < take; j++ {
						if _, _, ok := it.Next(); !ok {
							break
						}
					}
				}
			})
		}
	}
}

func corpusByName(name string) keySet {
	for _, ks := range corpus {
		if ks.name == name {
			return ks
		}
	}
	panic("unknown key set " + name)
}

// ---------------------------------------------------------------------------
// Footprint
// ---------------------------------------------------------------------------

var footprintFlag = flag.Bool("footprint", false, "run the steady-state footprint measurement")

// TestTreeFootprint reports bytes and heap objects per stored key.
//
// Not a testing.B benchmark: footprint is not a per-iteration quantity, and
// b.N scaling corrupts it. Keys are generated before the baseline is taken, so
// only the tree itself is counted.
func TestTreeFootprint(t *testing.T) {
	if !*footprintFlag {
		t.Skip("pass -footprint to run")
	}
	t.Logf("%-12s %-6s %12s %14s", "shape", "tree", "bytes/key", "objects/key")
	for _, ks := range corpus {
		rb, ro := measure(t, func() any { return buildRadix(ks.keys) }, len(ks.keys))
		ab, ao := measure(t, func() any { v := buildART(ks.keys); return &v }, len(ks.keys))
		t.Logf("%-12s %-6s %12.1f %14.2f", ks.name, "radix", rb, ro)
		t.Logf("%-12s %-6s %12.1f %14.2f   (%+.1f%% bytes, %+.1f%% objects)",
			ks.name, "art", ab, ao,
			(ab-rb)/rb*100, (ao-ro)/ro*100)
	}
}

func measure(tb testing.TB, build func() any, n int) (bytesPerKey, objsPerKey float64) {
	prev := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(prev)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&before)

	held := build()

	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(held)

	bytesPerKey = float64(after.HeapAlloc-before.HeapAlloc) / float64(n)
	objsPerKey = float64(after.HeapObjects-before.HeapObjects) / float64(n)
	return
}
