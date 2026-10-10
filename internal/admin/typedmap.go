package admin

import "sync"

// typedMap is a sync.Map with types.
type typedMap[K comparable, V any] struct{ m sync.Map }

func (t *typedMap[K, V]) Load(k K) (V, bool) {
	v, ok := t.m.Load(k)
	if !ok {
		var zero V
		return zero, false
	}
	return v.(V), true
}

func (t *typedMap[K, V]) Store(k K, v V) { t.m.Store(k, v) }
