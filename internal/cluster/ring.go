package cluster

import (
	"hash/fnv"
	"sort"
)

type Member struct {
	Name string
	// Addr is host:port of the member's proxy listener.
	Addr    string
	Version string
}

// Ring picks owners by rendezvous hashing: every member scores every key and the
// highest score wins. When a member leaves, only the keys it owned move.
type Ring struct {
	members []Member
}

func NewRing(members []Member) *Ring {
	ms := append([]Member(nil), members...)
	sort.Slice(ms, func(i, j int) bool { return ms[i].Name < ms[j].Name })
	return &Ring{members: ms}
}

func (r *Ring) Len() int {
	return len(r.members)
}

func (r *Ring) Members() []Member {
	return append([]Member(nil), r.members...)
}

func (r *Ring) Owner(key string) (Member, bool) {
	var best Member
	var bestScore uint64
	found := false
	for _, m := range r.members {
		s := score(m.Name, key)
		if !found || s > bestScore || (s == bestScore && m.Name < best.Name) {
			best, bestScore, found = m, s, true
		}
	}
	return best, found
}

func (r *Ring) Has(name string) bool {
	for _, m := range r.members {
		if m.Name == name {
			return true
		}
	}
	return false
}

func score(node, key string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(node))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(key))
	return mix(h.Sum64())
}

// mix is the splitmix64 finalizer. FNV alone leaves neighbouring inputs with
// correlated high bits, which skews who wins.
func mix(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}
