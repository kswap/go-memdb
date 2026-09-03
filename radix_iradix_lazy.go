// Copyright IBM Corp. 2015, 2026
// SPDX-License-Identifier: MPL-2.0

//go:build memdb_iradix_lazy

package memdb

import (
	iradix "github.com/hashicorp/go-memdb/internal/iradix"
)

// This file binds the index tree to internal/iradix, a fork of
// hashicorp/go-immutable-radix carrying one change: the notification channel on
// every node and leaf is created when a reader first asks for it rather than at
// construction. It is selected by -tags memdb_iradix_lazy.
//
// It exists to separate the two mechanisms that go-immutable-art combines.
// art wins partly by not allocating watch channels writers never need, and
// partly by packing an adaptive node into a single allocation. This arm keeps
// the first and drops the second, so the gap between it and radix_art.go is
// what the node layout is worth on its own -- and the gap between it and
// radix_iradix.go is what is available without taking a new dependency.
//
// The fork is API-identical to upstream, so the aliases and every call site in
// memdb.go and txn.go are unchanged.

type (
	idxTree            = iradix.Tree
	idxTxn             = iradix.Txn
	idxIterator        = iradix.Iterator
	idxReverseIterator = iradix.ReverseIterator
)

// newIdxTree returns an empty index tree.
func newIdxTree() *idxTree {
	return iradix.New()
}
