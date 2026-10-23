package kcond

import (
	"hash/maphash"
	"math/bits"
	"slices"
	"sync"
	"sync/atomic"
	"weak"
)

// The constructors share nodes: one live node holds each expression, so
// equal expressions are the same pointer. The table holds the nodes
// weakly, as no expression is needed for the whole of a run.
var (
	table  [64]shard
	seed   = maphash.MakeSeed()
	lastID atomic.Uint64
)

// shard is the part of the table for the hashes whose top bits are its
// index.
type shard struct {
	mu    sync.Mutex
	nodes map[uint64][]weak.Pointer[Expr]
	n     int // the entries, dead ones included
	live  int // the entries the last sweep kept
}

// minSweep is the number of entries a shard grows by before it drops its
// dead ones.
const minSweep = 1 << 10

func newID() uint64 { return lastID.Add(1) }

// intern returns the node of a variable, a Not or an AND or OR of args,
// args being in their canonical order, made when no live node is.
func intern(op Op, name string, st State, args []*Expr) *Expr {
	h := uint64(op)<<8 | uint64(st)
	if name != "" {
		h ^= maphash.String(seed, name)
	}
	for _, a := range args {
		h = (bits.RotateLeft64(h, 5) ^ a.id) * 0x9e3779b97f4a7c15
	}
	s := &table[h>>58]
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, w := range s.nodes[h] {
		if n := w.Value(); n != nil && n.Op == op && n.Name == name && n.State == st && slices.Equal(n.Args, args) {
			return n
		}
	}
	n := &Expr{Op: op, Name: name, State: st, Args: args, id: newID()}
	n.leaf, n.size = n.measure()
	if s.nodes == nil {
		s.nodes = map[uint64][]weak.Pointer[Expr]{}
	}
	s.nodes[h] = append(s.nodes[h], weak.Make(n))
	if s.n++; s.n > 2*s.live+minSweep {
		s.sweep()
	}
	return n
}

// sweep drops the entries whose node is gone.
func (s *shard) sweep() {
	s.n = 0
	for h, ws := range s.nodes {
		ws = slices.DeleteFunc(ws, func(w weak.Pointer[Expr]) bool { return w.Value() == nil })
		if len(ws) == 0 {
			delete(s.nodes, h)
		} else {
			s.nodes[h] = ws
		}
		s.n += len(ws)
	}
	s.live = s.n
}
