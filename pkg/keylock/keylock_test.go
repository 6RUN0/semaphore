package keylock

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestKeyLock_SameKeySerializes(t *testing.T) {
	l := &KeyLock{}

	unlock := l.Lock("repo_1")

	acquired := make(chan struct{})
	go func() {
		u := l.Lock("repo_1")
		close(acquired)
		u()
	}()

	select {
	case <-acquired:
		assert.Fail(t, "second Lock acquired while first is held")
	case <-time.After(50 * time.Millisecond):
	}

	unlock()

	select {
	case <-acquired:
	case <-time.After(time.Second):
		assert.Fail(t, "second Lock not acquired after unlock")
	}
}

func TestKeyLock_DifferentKeysIndependent(t *testing.T) {
	l := &KeyLock{}

	unlockA := l.Lock("repo_1")
	defer unlockA()

	acquired := make(chan struct{})
	go func() {
		u := l.Lock("repo_2")
		u()
		close(acquired)
	}()

	select {
	case <-acquired:
	case <-time.After(time.Second):
		assert.Fail(t, "different key must not block")
	}
}

// The map has to shrink again: the checkout key carries a hash of the branch
// name, so every branch a project ever points a repository at would otherwise
// leave an entry behind for as long as the server runs.
func TestKeyLock_ForgetsKeyNobodyHolds(t *testing.T) {
	l := &KeyLock{}

	unlock := l.Lock("repo_1_branch_a")
	assert.Len(t, l.locks, 1)

	unlock()

	assert.Empty(t, l.locks, "an unheld key must not stay in the map")
}

// A waiter is counted before it blocks, so the holder's unlock must not drop
// the entry from under it — both callers have to end up on the same mutex, or
// the key stops serializing anything.
func TestKeyLock_KeepsKeyWhileSomebodyWaits(t *testing.T) {
	l := &KeyLock{}

	unlock := l.Lock("repo_1")

	waiting := make(chan func())
	go func() {
		waiting <- l.Lock("repo_1")
	}()

	// The waiter has to be inside Lock before the holder releases; without the
	// wait this would test the uncontended path instead.
	assert.Eventually(t, func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()

		return l.locks["repo_1"] != nil && l.locks["repo_1"].users == 2
	}, time.Second, time.Millisecond, "waiter was never counted")

	unlock()

	select {
	case u := <-waiting:
		assert.Len(t, l.locks, 1, "the key must survive while its waiter holds it")
		u()
		assert.Empty(t, l.locks)
	case <-time.After(time.Second):
		assert.Fail(t, "waiter never acquired the key")
	}
}

func TestKeyLock_ReuseAfterUnlock(t *testing.T) {
	l := &KeyLock{}

	unlock := l.Lock("repo_1")
	unlock()

	done := make(chan struct{})
	go func() {
		u := l.Lock("repo_1")
		u()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		assert.Fail(t, "key must be lockable again after unlock")
	}
}
