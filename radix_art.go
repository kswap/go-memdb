// Copyright IBM Corp. 2015, 2026
// SPDX-License-Identifier: MPL-2.0

//go:build memdb_art

package memdb

import (
	art "github.com/kswap/go-immutable-art"
)

// This file binds the index tree to kswap/go-immutable-art, an immutable
// adaptive radix tree. It is selected by -tags memdb_art; the default build
// uses hashicorp/go-immutable-radix via radix_iradix.go.
//
// art is generic where iradix is not, so the trees are instantiated at any.
// interface{} and any are the same type, which is what lets the call sites in
// memdb.go and txn.go stay identical across both backends.

type (
	idxTree            = art.Tree[any]
	idxTxn             = art.Txn[any]
	idxIterator        = art.Iterator[any]
	idxReverseIterator = art.ReverseIterator[any]
)

// newIdxTree returns an empty index tree.
func newIdxTree() *idxTree {
	return art.New[any]()
}
