package keylock

import "sync"

// KeyLock serializes work on a shared resource identified by a string key.
// It exists to prevent concurrent git operations (pull/clone/checkout) on the
// same working directory: whoever shares one copy on disk corrupts it with a
// concurrent `git pull`.
//
// The zero value is ready to use.
type KeyLock struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// Lock blocks until the mutex for the given key is acquired and returns the
// unlock function.
//
// Note: entries are never removed — the map is bounded by the number of
// distinct directories (repos × templates), which is negligible.
func (l *KeyLock) Lock(key string) func() {
	l.mu.Lock()
	if l.locks == nil {
		l.locks = make(map[string]*sync.Mutex)
	}
	m, ok := l.locks[key]
	if !ok {
		m = &sync.Mutex{}
		l.locks[key] = m
	}
	l.mu.Unlock()

	m.Lock()
	return m.Unlock
}
