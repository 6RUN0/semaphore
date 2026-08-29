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
	locks map[string]*keyEntry
}

// keyEntry is one key's mutex plus the number of callers that hold it or are
// waiting for it. The count is what allows the entry to be dropped again:
// keys are not a fixed set any more — the inventory snapshot checkout keys on
// the repository *and* its branch, so a renamed branch mints a new key that the
// old one would otherwise outlive for the lifetime of the process.
type keyEntry struct {
	mu sync.Mutex

	// users is guarded by KeyLock.mu, not by mu: it is read and written while
	// deciding whether the entry may leave the map, which happens outside the
	// entry's own critical section.
	users int
}

// Lock blocks until the mutex for the given key is acquired and returns the
// unlock function.
//
// The returned function must be called exactly once, as with sync.Mutex: a
// second call panics rather than corrupting the bookkeeping.
func (l *KeyLock) Lock(key string) func() {
	l.mu.Lock()
	if l.locks == nil {
		l.locks = make(map[string]*keyEntry)
	}
	entry, ok := l.locks[key]
	if !ok {
		entry = &keyEntry{}
		l.locks[key] = entry
	}
	entry.users++
	l.mu.Unlock()

	entry.mu.Lock()

	return func() {
		// Released first, so that a waiter enters as early as it would have
		// before: it is already counted in users, which is what keeps the entry
		// in the map while this call drops its own reference.
		entry.mu.Unlock()

		l.mu.Lock()
		entry.users--
		if entry.users == 0 {
			delete(l.locks, key)
		}
		l.mu.Unlock()
	}
}
