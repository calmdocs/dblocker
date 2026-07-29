package dblocker

import (
	"fmt"
	"sync"
)

// DefaultShardCount is the number of lock shards used by a Store unless
// overridden with Options.ShardCount.
//
// Instead of guarding the whole id -> Group map with a single store-wide
// mutex, the map is split ("striped") across this many independent shards,
// each with its own mutex.  An id is hashed to pick its shard, so requests
// for different ids almost never contend on the same lock, and the hot
// mutex cache line is no longer bounced between every core.
// See https://strebkov.dev/posts/shard-your-locks/ for benchmarks of this
// pattern: a 256-way striped map was up to 8x faster than a single
// sync.Mutex at 8 cores, and kept scaling as cores were added.
const DefaultShardCount = 256

// storeShard is a single lock stripe: one mutex guarding one slice of the
// id -> Group map.  All operations for a given id (group lookup/creation,
// request counting, and group teardown) use the same shard, so the
// correctness argument is identical to the old single-mutex design - it is
// simply repeated per shard.
type storeShard struct {
	mu sync.Mutex
	m  map[interface{}]*Group

	// Pad each shard towards its own CPU cache line so that locking one
	// shard does not invalidate the cache line of its neighbours
	// (false sharing).
	_ [40]byte
}

// FNV-1a constants (see hash/fnv).  Inlined here so hashing an id does not
// allocate a hash.Hash64 on every database request.
const (
	fnvOffset64 uint64 = 14695981039346656037
	fnvPrime64  uint64 = 1099511628211
)

func fnvHashString(s string) uint64 {
	h := fnvOffset64
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= fnvPrime64
	}
	return h
}

func fnvHashUint64(v uint64) uint64 {
	h := fnvOffset64
	for i := 0; i < 8; i++ {
		h ^= v & 0xff
		h *= fnvPrime64
		v >>= 8
	}
	return h
}

// shardFor hashes an id to its shard.  Common id types (strings and
// integers) are hashed directly; anything else falls back to hashing the
// fmt "%v" representation.  Two ids that are equal as map keys always hash
// to the same shard; unequal ids sharing a shard is harmless because each
// shard's map is still keyed by the id itself.
func (s *Store) shardFor(id interface{}) *storeShard {
	var h uint64
	switch v := id.(type) {
	case string:
		h = fnvHashString(v)
	case int:
		h = fnvHashUint64(uint64(v))
	case int8:
		h = fnvHashUint64(uint64(v))
	case int16:
		h = fnvHashUint64(uint64(v))
	case int32:
		h = fnvHashUint64(uint64(v))
	case int64:
		h = fnvHashUint64(uint64(v))
	case uint:
		h = fnvHashUint64(uint64(v))
	case uint8:
		h = fnvHashUint64(uint64(v))
	case uint16:
		h = fnvHashUint64(uint64(v))
	case uint32:
		h = fnvHashUint64(uint64(v))
	case uint64:
		h = fnvHashUint64(v)
	default:
		h = fnvHashString(fmt.Sprintf("%v", v))
	}
	return s.shards[h&s.shardMask]
}

// nextPowerOfTwo rounds n up to the next power of two so the shard index
// can be computed with a mask instead of a modulo.
func nextPowerOfTwo(n int) int {
	p := 1
	for p < n {
		p <<= 1
	}
	return p
}
