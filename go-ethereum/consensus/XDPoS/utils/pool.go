// Copyright (c) 2024 XDC Network
// Pool implementation for XDPoS 2.0 vote and timeout messages

package utils

import (
	"sync"

	"github.com/ethereum/go-ethereum/common"
)

// PoolObj is the interface for objects that can be added to the pool
type PoolObj interface {
	Hash() common.Hash
	PoolKey() string
	GetSigner() common.Address
	SetSigner(common.Address)
}

// Pool is a thread-safe message pool for votes and timeouts
type Pool struct {
	lock sync.RWMutex
	// Key: PoolKey (round:gapNumber:number:hash for votes, round:gapNumber for timeouts)
	// Value: map of message hash -> PoolObj
	pool map[string]map[common.Hash]PoolObj
}

// NewPool creates a new Pool
func NewPool() *Pool {
	return &Pool{
		pool: make(map[string]map[common.Hash]PoolObj),
	}
}

// Add adds an object to the pool
// Returns the count of objects with the same pool key and all objects with that key
func (p *Pool) Add(obj PoolObj) (int, map[common.Hash]PoolObj) {
	p.lock.Lock()
	defer p.lock.Unlock()

	key := obj.PoolKey()
	hash := obj.Hash()

	if p.pool[key] == nil {
		p.pool[key] = make(map[common.Hash]PoolObj)
	}

	// Only add if not already present
	if _, exists := p.pool[key][hash]; !exists {
		p.pool[key][hash] = obj
	}

	// Return a copy of the pool content for this key
	result := make(map[common.Hash]PoolObj)
	for k, v := range p.pool[key] {
		result[k] = v
	}

	return len(p.pool[key]), result
}

// Get returns the entire pool
func (p *Pool) Get() map[string]map[common.Hash]PoolObj {
	p.lock.RLock()
	defer p.lock.RUnlock()

	result := make(map[string]map[common.Hash]PoolObj)
	for key, objects := range p.pool {
		result[key] = make(map[common.Hash]PoolObj)
		for hash, obj := range objects {
			result[key][hash] = obj
		}
	}
	return result
}

// GetByPoolKey returns all objects for a specific pool key
func (p *Pool) GetByPoolKey(key string) map[common.Hash]PoolObj {
	p.lock.RLock()
	defer p.lock.RUnlock()

	if p.pool[key] == nil {
		return nil
	}

	result := make(map[common.Hash]PoolObj)
	for hash, obj := range p.pool[key] {
		result[hash] = obj
	}
	return result
}

// PoolObjKeysList returns all pool keys
func (p *Pool) PoolObjKeysList() []string {
	p.lock.RLock()
	defer p.lock.RUnlock()

	keys := make([]string, 0, len(p.pool))
	for key := range p.pool {
		keys = append(keys, key)
	}
	return keys
}

// Clear clears the entire pool
func (p *Pool) Clear() {
	p.lock.Lock()
	defer p.lock.Unlock()
	p.pool = make(map[string]map[common.Hash]PoolObj)
}

// ClearByPoolKey clears objects for a specific pool key
func (p *Pool) ClearByPoolKey(key string) {
	p.lock.Lock()
	defer p.lock.Unlock()
	delete(p.pool, key)
}

// Size returns the total number of objects in the pool
func (p *Pool) Size() int {
	p.lock.RLock()
	defer p.lock.RUnlock()

	count := 0
	for _, objects := range p.pool {
		count += len(objects)
	}
	return count
}


// GetObjsByKey returns all objects matching the given pool key.
func (p *Pool) GetObjsByKey(key string) map[common.Hash]PoolObj {
	p.lock.RLock()
	defer p.lock.RUnlock()

	result := make(map[common.Hash]PoolObj)
	if items, ok := p.pool[key]; ok {
		for hash, obj := range items {
			result[hash] = obj
		}
	}
	return result
}
