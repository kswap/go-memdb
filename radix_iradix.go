// Copyright IBM Corp. 2015, 2026
// SPDX-License-Identifier: MPL-2.0

//go:build !memdb_art && !memdb_iradix_lazy

package memdb

import (
	iradix "github.com/hashicorp/go-immutable-radix"
)

// This file binds the index tree to hashicorp/go-immutable-radix, the default.
// Build with -tags memdb_art to bind it to kswap/go-immutable-art instead; see
// radix_art.go. The two backends expose the same method set, and iradix's
// interface{} is the same type as art.Tree[any]'s any, so every call site in
// memdb.go and txn.go compiles unchanged against either.

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
