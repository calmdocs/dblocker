package dblocker

import (
	"fmt"
	"sync"
)

// shardCount is the number of independently locked shards in a Store.
//
// The Store map is sharded so that requests for different ids rarely contend
// on the same mutex (see https://strebkov.dev/posts/shard-your-locks/ for
// benchmarks of this pattern: moving from one lock to 256 shards scales
// near-linearly with cores, while a single mutex flatlines).  256 is the
// point of diminishing returns in those benchmarks.  It must be a power of
// two so that a bitmask can select the shard.
const shardCount = 256

// shard is one independently locked slice of the Store's id -> *Group map.
// The trailing padding keeps adjacent shards on separate cache lines so that
// locking one shard does not invalidate the cache line of its neighbours
// (false sharing).
type shard struct {
	sync.Mutex
	m map[interface{}]*Group

	_ [64 - 16]byte
}

// shardFor returns the shard responsible for id.  The same id always maps to
// the same shard, so all lifecycle operations for a Group (create, count,
// delete) serialise on a single shard mutex.
func (s *Store) shardFor(id interface{}) *shard {
	return &s.shards[shardIndex(id)&(shardCount-1)]
}

// shardIndex hashes an id of any comparable type to a shard index.  Common
// key types (strings and integers) are hashed directly; everything else is
// formatted with fmt.Sprintf first.  A hash collision only means two ids
// share a shard mutex — Group lookup is still exact via the shard map.
func shardIndex(id interface{}) uint32 {
	switch v := id.(type) {
	case string:
		return fnv32a(v)
	case int:
		return mix64(uint64(v))
	case int8:
		return mix64(uint64(v))
	case int16:
		return mix64(uint64(v))
	case int32:
		return mix64(uint64(v))
	case int64:
		return mix64(uint64(v))
	case uint:
		return mix64(uint64(v))
	case uint8:
		return mix64(uint64(v))
	case uint16:
		return mix64(uint64(v))
	case uint32:
		return mix64(uint64(v))
	case uint64:
		return mix64(v)
	default:
		return fnv32a(fmt.Sprintf("%v", id))
	}
}

// fnv32a is the FNV-1a hash of s.
func fnv32a(s string) uint32 {
	const (
		offset32 = 2166136261
		prime32  = 16777619
	)
	h := uint32(offset32)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= prime32
	}
	return h
}

// mix64 is a splitmix64-style finaliser that spreads integer ids evenly
// across shards even when the ids themselves are sequential.
func mix64(x uint64) uint32 {
	x ^= x >> 33
	x *= 0xff51afd7ed558ccd
	x ^= x >> 33
	x *= 0xc4ceb9fe1a85ec53
	x ^= x >> 33
	return uint32(x)
}
