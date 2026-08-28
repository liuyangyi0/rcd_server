package app

import (
	"hash/fnv"
	"sync"

	"rcd_manager/internal/domain"
)

const sampleShardCount = 64

type sampleCache struct {
	shards [sampleShardCount]sampleShard
}

type sampleShard struct {
	sync.RWMutex
	values map[string]domain.Sample
}

func newSampleCache() *sampleCache {
	cache := &sampleCache{}
	for index := range cache.shards {
		cache.shards[index].values = make(map[string]domain.Sample)
	}
	return cache
}

// Put ignores notifications from an older lower-session epoch and late values
// from a parallel observation path within the same epoch.
func (c *sampleCache) Put(sourceID string, key domain.NodeKey, sample domain.Sample) bool {
	cacheKey := makeSampleKey(sourceID, key)
	shard := c.shard(cacheKey)
	shard.Lock()
	defer shard.Unlock()
	if current, ok := shard.values[cacheKey]; ok {
		if current.Epoch > sample.Epoch {
			return false
		}
		if current.Epoch == sample.Epoch && !current.SourceTimestamp.IsZero() && !sample.SourceTimestamp.IsZero() &&
			sample.SourceTimestamp.Before(current.SourceTimestamp) {
			return false
		}
	}
	shard.values[cacheKey] = sample
	return true
}

func (c *sampleCache) Get(sourceID string, key domain.NodeKey) (domain.Sample, bool) {
	cacheKey := makeSampleKey(sourceID, key)
	shard := c.shard(cacheKey)
	shard.RLock()
	defer shard.RUnlock()
	value, ok := shard.values[cacheKey]
	return value, ok
}

func (c *sampleCache) DeleteSource(sourceID string) {
	prefix := sourceID + "\x00"
	for index := range c.shards {
		shard := &c.shards[index]
		shard.Lock()
		for key := range shard.values {
			if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
				delete(shard.values, key)
			}
		}
		shard.Unlock()
	}
}

func (c *sampleCache) shard(key string) *sampleShard {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(key))
	return &c.shards[hash.Sum32()%sampleShardCount]
}

func makeSampleKey(sourceID string, key domain.NodeKey) string {
	return sourceID + "\x00" + key.NamespaceURI + "\x00" + key.Identifier
}

func zeroValue(dataType domain.DataType, valueRank ...int32) any {
	if len(valueRank) > 0 && valueRank[0] >= 0 {
		switch dataType {
		case domain.DataTypeBoolean:
			return []bool{}
		case domain.DataTypeUInt32:
			return []uint32{}
		case domain.DataTypeFloat:
			return []float32{}
		case domain.DataTypeDouble:
			return []float64{}
		case domain.DataTypeString:
			return []string{}
		default:
			return []any{}
		}
	}
	switch dataType {
	case domain.DataTypeBoolean:
		return false
	case domain.DataTypeUInt32:
		return uint32(0)
	case domain.DataTypeFloat:
		return float32(0)
	case domain.DataTypeDouble:
		return float64(0)
	case domain.DataTypeString:
		return ""
	default:
		return nil
	}
}
