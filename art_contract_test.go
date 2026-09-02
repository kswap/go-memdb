// Copyright IBM Corp. 2015, 2026
// SPDX-License-Identifier: MPL-2.0

package memdb

import (
	"testing"
	"time"
)

// These tests pin the index-tree behaviours that memdb depends on but that the
// upstream suite never exercises directly. They are backend-neutral on purpose:
// they must pass identically with and without -tags memdb_art, and a failure
// here localises a divergence to the tree rather than to Consul.

type contractObj struct {
	ID   string
	Fam  string
	Solo string
}

func contractSchema() *DBSchema {
	return &DBSchema{
		Tables: map[string]*TableSchema{
			"main": {
				Name: "main",
				Indexes: map[string]*IndexSchema{
					"id": {
						Name:    "id",
						Unique:  true,
						Indexer: &StringFieldIndex{Field: "ID"},
					},
					// The index DeletePrefix scans and then prefix-deletes.
					"fam": {
						Name:    "fam",
						Indexer: &StringFieldIndex{Field: "Fam"},
					},
					// A sibling index whose values share no prefix with fam's,
					// so a prefix delete against fam never matches here.
					"solo": {
						Name:    "solo",
						Indexer: &StringFieldIndex{Field: "Solo"},
					},
				},
			},
		},
	}
}

// TestART_DeletePrefix_SiblingIndexUnmatched covers the one divergence that can
// reach memdb: go-immutable-art reports DeletePrefix over an empty subtree as
// false where go-immutable-radix reports true. memdb panics on a false return
// (txn.go, "matched some entries but DeletePrefix did not delete any"), so this
// asserts that deleting through an index whose sibling holds no matching prefix
// still completes, and that the sibling's rows are cleaned up rather than left
// dangling.
func TestART_DeletePrefix_SiblingIndexUnmatched(t *testing.T) {
	db, err := NewMemDB(contractSchema())
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	txn := db.Txn(true)
	rows := []*contractObj{
		{ID: "a1", Fam: "team/alpha", Solo: "zzz-1"},
		{ID: "a2", Fam: "team/alpha", Solo: "zzz-2"},
		{ID: "b1", Fam: "team/beta", Solo: "zzz-3"},
	}
	for _, row := range rows {
		if err := txn.Insert("main", row); err != nil {
			t.Fatalf("err: %v", err)
		}
	}
	txn.Commit()

	// Deleting the team/alpha prefix must report a deletion and must not panic,
	// even though the "solo" index has nothing under that prefix.
	txn = db.Txn(true)
	ok, err := txn.DeletePrefix("main", "fam_prefix", "team/alpha")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !ok {
		t.Fatalf("expected the prefix delete to report a deletion")
	}
	txn.Commit()

	// Both matching rows are gone from every index, and the non-matching row
	// survives in all of them.
	txn = db.Txn(false)
	defer txn.Abort()

	for _, id := range []string{"a1", "a2"} {
		raw, err := txn.First("main", "id", id)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if raw != nil {
			t.Fatalf("row %q should be gone from the id index", id)
		}
	}
	for _, solo := range []string{"zzz-1", "zzz-2"} {
		raw, err := txn.First("main", "solo", solo)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if raw != nil {
			t.Fatalf("solo %q should be gone from the sibling index", solo)
		}
	}
	raw, err := txn.First("main", "solo", "zzz-3")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if raw == nil {
		t.Fatalf("the non-matching row should survive in the sibling index")
	}

	// A second delete of the same prefix now matches nothing. memdb must report
	// false without reaching the tree's DeletePrefix at all.
	txn = db.Txn(true)
	ok, err = txn.DeletePrefix("main", "fam_prefix", "team/alpha")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ok {
		t.Fatalf("expected no deletion the second time round")
	}
	txn.Abort()
}

// TestART_WatchPrefix_MissThenAppear is the subtlest contract in the swap.
// SeekPrefixWatch must return the channel of the deepest node it reached even
// when the prefix matches nothing, so a blocking query that currently sees an
// empty result still wakes when a matching row first appears. Every Consul
// blocking query on an empty service depends on this.
func TestART_WatchPrefix_MissThenAppear(t *testing.T) {
	db, err := NewMemDB(contractSchema())
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Seed an unrelated row so the tree is not empty; the interesting case is a
	// prefix that misses partway down a populated tree, not one that misses at
	// the root.
	txn := db.Txn(true)
	if err := txn.Insert("main", &contractObj{ID: "seed", Fam: "team/beta", Solo: "s"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	txn.Commit()

	// Watch a prefix that matches nothing today.
	txn = db.Txn(false)
	it, err := txn.Get("main", "fam_prefix", "team/alpha")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if raw := it.Next(); raw != nil {
		t.Fatalf("expected no results, got %#v", raw)
	}
	ws := NewWatchSet()
	ws.Add(it.WatchCh())
	if it.WatchCh() == nil {
		t.Fatalf("a missed prefix must still yield a watch channel")
	}
	txn.Abort()

	// A row appearing under that prefix must wake the watcher.
	txn = db.Txn(true)
	if err := txn.Insert("main", &contractObj{ID: "a1", Fam: "team/alpha", Solo: "t"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	txn.Commit()

	if timedOut := ws.Watch(time.After(2 * time.Second)); timedOut {
		t.Fatalf("watcher on a previously-empty prefix never woke")
	}
}

// TestART_WatchPrefix_UnrelatedWriteIsQuiet is the negative control for the test
// above: a watch that fires for every write proves nothing. A write landing well
// away from the watched prefix must leave the watcher asleep.
func TestART_WatchPrefix_UnrelatedWriteIsQuiet(t *testing.T) {
	db, err := NewMemDB(contractSchema())
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	txn := db.Txn(true)
	if err := txn.Insert("main", &contractObj{ID: "a1", Fam: "team/alpha", Solo: "s"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	txn.Commit()

	txn = db.Txn(false)
	it, err := txn.Get("main", "fam_prefix", "team/alpha")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	ws := NewWatchSet()
	ws.Add(it.WatchCh())
	txn.Abort()

	txn = db.Txn(true)
	if err := txn.Insert("main", &contractObj{ID: "z9", Fam: "zulu/nine", Solo: "u"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	txn.Commit()

	if timedOut := ws.Watch(time.After(150 * time.Millisecond)); !timedOut {
		t.Fatalf("a write under an unrelated prefix woke the watcher")
	}
}

// TestART_GetWatch_ChannelOnMiss covers FirstWatch's miss path: a point lookup
// that finds nothing must still hand back a live channel, which memdb returns
// straight to the caller alongside a nil object.
func TestART_GetWatch_ChannelOnMiss(t *testing.T) {
	db, err := NewMemDB(contractSchema())
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	txn := db.Txn(true)
	if err := txn.Insert("main", &contractObj{ID: "aardvark", Fam: "f", Solo: "s"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	txn.Commit()

	txn = db.Txn(false)
	ch, raw, err := txn.FirstWatch("main", "id", "aardwolf")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if raw != nil {
		t.Fatalf("expected a miss, got %#v", raw)
	}
	if ch == nil {
		t.Fatalf("a missed point lookup must still yield a watch channel")
	}
	ws := NewWatchSet()
	ws.Add(ch)
	txn.Abort()

	txn = db.Txn(true)
	if err := txn.Insert("main", &contractObj{ID: "aardwolf", Fam: "f", Solo: "s2"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	txn.Commit()

	if timedOut := ws.Watch(time.After(2 * time.Second)); timedOut {
		t.Fatalf("watcher on a missed key never woke when the key appeared")
	}
}

// TestART_LongestPrefix_NullTerminatedKeysNeverMatch pins the documented
// interaction between LongestPrefix and StringFieldIndex. The indexer appends a
// null terminator to every stored key while the _prefix argument form strips it,
// so the walk stops one byte short of a leaf and matches nothing — not even an
// exact key. memdb's own doc comment warns about this, and TestTxn_InsertGet_
// LongestPrefix sidesteps it by testing against CustomIndex instead, which
// leaves the case untested upstream.
//
// It is pinned here because it is quietly load-bearing: a replacement tree that
// "fixed" LongestPrefix to match the shorter key would change memdb's results
// without failing any existing test.
func TestART_LongestPrefix_NullTerminatedKeysNeverMatch(t *testing.T) {
	db, err := NewMemDB(contractSchema())
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	txn := db.Txn(true)
	for _, id := range []string{"alpha", "alphabet", "alphabetical", "beta"} {
		if err := txn.Insert("main", &contractObj{ID: id, Fam: "f-" + id, Solo: "s-" + id}); err != nil {
			t.Fatalf("err: %v", err)
		}
	}
	txn.Commit()

	txn = db.Txn(false)
	defer txn.Abort()

	for _, arg := range []string{
		"alpha",          // an exact key
		"alphabet",       // another exact key
		"alphabetical",   // the longest exact key
		"alphabetically", // an extension of a stored key
		"alph",           // a proper prefix of a stored key
		"a",
		"",
		"gamma", // unrelated
	} {
		raw, err := txn.LongestPrefix("main", "id_prefix", arg)
		if err != nil {
			t.Fatalf("arg %q: err: %v", arg, err)
		}
		if raw != nil {
			t.Fatalf("arg %q: null-terminated keys must never match, got %#v", arg, raw)
		}
	}
}

// TestART_IterateWhileDeleting exercises Clone's obligation to stop the parent
// transaction mutating nodes in place. go-immutable-radix meets it by dropping
// its writable-node cache; go-immutable-art meets it by burning the txn id. A
// backend that met it by neither would corrupt the live iterator here.
func TestART_IterateWhileDeleting(t *testing.T) {
	db, err := NewMemDB(contractSchema())
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	const n = 500
	txn := db.Txn(true)
	for i := 0; i < n; i++ {
		obj := &contractObj{
			ID:   string(rune('a'+i%26)) + "-" + itoa(i),
			Fam:  "team/alpha",
			Solo: "s-" + itoa(i),
		}
		if err := txn.Insert("main", obj); err != nil {
			t.Fatalf("err: %v", err)
		}
	}
	txn.Commit()

	// Iterate and delete within a single write transaction. The iterator was
	// built before the deletes, so it must still yield all n rows.
	txn = db.Txn(true)
	it, err := txn.Get("main", "fam", "team/alpha")
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	seen := 0
	for raw := it.Next(); raw != nil; raw = it.Next() {
		seen++
		if err := txn.Delete("main", raw); err != nil {
			t.Fatalf("err: %v", err)
		}
	}
	if seen != n {
		t.Fatalf("expected to walk %d rows, walked %d", n, seen)
	}
	txn.Commit()

	txn = db.Txn(false)
	defer txn.Abort()
	it, err = txn.Get("main", "fam", "team/alpha")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if raw := it.Next(); raw != nil {
		t.Fatalf("expected the table to be empty, found %#v", raw)
	}
}

// TestART_SnapshotWritesDoNotWakeThePrimary covers the reason writableIndex
// gates TrackMutate on db.primary: a snapshot shares its nodes, and therefore
// its watch channels, with the primary. A write committed through the snapshot
// must stay invisible to the primary and must leave the primary's watchers
// asleep, because the rows behind those channels never changed there.
//
// The upstream suite covers the visibility half of this (isolation_test.go's
// "snapshot commits are unobservable") but not the watch half.
func TestART_SnapshotWritesDoNotWakeThePrimary(t *testing.T) {
	db, err := NewMemDB(contractSchema())
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	txn := db.Txn(true)
	if err := txn.Insert("main", &contractObj{ID: "a1", Fam: "team/alpha", Solo: "s1"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	txn.Commit()

	// Watch the primary.
	txn = db.Txn(false)
	it, err := txn.Get("main", "fam_prefix", "team/")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	ws := NewWatchSet()
	ws.Add(it.WatchCh())
	txn.Abort()

	// Write through a snapshot.
	snap := db.Snapshot()
	snapTxn := snap.Txn(true)
	if err := snapTxn.Insert("main", &contractObj{ID: "a2", Fam: "team/alpha", Solo: "s2"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	snapTxn.Commit()

	// The primary neither sees the row...
	txn = db.Txn(false)
	defer txn.Abort()
	it, err = txn.Get("main", "fam_prefix", "team/")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	count := 0
	for raw := it.Next(); raw != nil; raw = it.Next() {
		count++
	}
	if count != 1 {
		t.Fatalf("primary should still hold 1 row, holds %d", count)
	}

	// ...nor wakes for it.
	if timedOut := ws.Watch(time.After(150 * time.Millisecond)); !timedOut {
		t.Fatalf("a commit through a snapshot woke a watcher on the primary")
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
