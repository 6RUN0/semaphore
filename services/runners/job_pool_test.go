package runners

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/semaphoreui/semaphore/db"
	"github.com/semaphoreui/semaphore/pkg/task_logger"
	"github.com/semaphoreui/semaphore/services/tasks"
	"github.com/semaphoreui/semaphore/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestJob(id int) *job {
	return &job{
		job:    &tasks.LocalExecutor{Task: db.Task{ID: id}},
		taskID: id,
	}
}

func initConfig(t *testing.T) {
	prevCfg := util.Config
	t.Cleanup(func() { util.Config = prevCfg })

	util.Config = &util.ConfigType{
		Runner: &util.RunnerConfig{
			Executor:   &util.ExecutorConfig{},
			Connection: &util.RunnerConnectionConfig{},
		},
	}

}

func TestJobPool_QueueOrder(t *testing.T) {
	initConfig(t)

	p := NewJobPool(nil)

	assert.Equal(t, 0, p.queueLen())

	p.enqueue(newTestJob(1))
	p.enqueue(newTestJob(2))
	p.enqueue(newTestJob(3))

	assert.Equal(t, 3, p.queueLen())
	assert.True(t, p.existsInQueue(2))
	assert.False(t, p.existsInQueue(99))

	// FIFO order.
	j, ok := p.dequeue()
	require.True(t, ok)
	assert.Equal(t, 1, j.taskID)

	j, ok = p.dequeue()
	require.True(t, ok)
	assert.Equal(t, 2, j.taskID)

	j, ok = p.dequeue()
	require.True(t, ok)
	assert.Equal(t, 3, j.taskID)

	_, ok = p.dequeue()
	assert.False(t, ok)
}

func TestJobPool_RunningJobsLifecycle(t *testing.T) {
	initConfig(t)

	p := NewJobPool(nil)

	assert.Equal(t, 0, p.runningJobsCount())
	assert.Nil(t, p.getRunningJob(1))

	rj := &runningJob{
		job:    &tasks.LocalExecutor{Task: db.Task{ID: 1}},
		status: task_logger.TaskRunningStatus,
	}
	p.addRunningJob(1, rj)

	assert.Equal(t, 1, p.runningJobsCount())
	assert.Same(t, rj, p.getRunningJob(1))

	snapshot := p.snapshotRunningJobs()
	assert.Len(t, snapshot, 1)
	// Snapshot is a copy: deleting from it must not affect the pool.
	delete(snapshot, 1)
	assert.Equal(t, 1, p.runningJobsCount())

	p.deleteRunningJob(1)
	assert.Equal(t, 0, p.runningJobsCount())
	assert.Nil(t, p.getRunningJob(1))
}

func TestJobPool_HasRunningJobs(t *testing.T) {
	initConfig(t)

	p := NewJobPool(nil)
	assert.False(t, p.hasRunningJobs())

	p.addRunningJob(1, &runningJob{
		job:    &tasks.LocalExecutor{Task: db.Task{ID: 1}},
		status: task_logger.TaskSuccessStatus, // finished
	})
	assert.False(t, p.hasRunningJobs())

	p.addRunningJob(2, &runningJob{
		job:    &tasks.LocalExecutor{Task: db.Task{ID: 2}},
		status: task_logger.TaskRunningStatus, // not finished
	})
	assert.True(t, p.hasRunningJobs())
}

func TestJobPool_ApplyTerminatedJobs(t *testing.T) {
	initConfig(t)

	p := NewJobPool(nil)

	// Running job: must be emergency stopped and removed.
	lj := &tasks.LocalExecutor{Task: db.Task{ID: 1}}
	rj := &runningJob{job: lj, status: task_logger.TaskRunningStatus}
	lj.Logger = rj
	p.addRunningJob(1, rj)

	// Already finished job: must be removed without a status change or kill.
	lj2 := &tasks.LocalExecutor{Task: db.Task{ID: 2}}
	rj2 := &runningJob{job: lj2, status: task_logger.TaskSuccessStatus}
	lj2.Logger = rj2
	p.addRunningJob(2, rj2)

	// Unknown task ID (99) must be a no-op.
	p.applyTerminatedJobs([]int{1, 2, 99})

	assert.Equal(t, task_logger.TaskStoppedStatus, rj.getStatus())
	assert.True(t, lj.IsKilled())

	assert.Equal(t, task_logger.TaskSuccessStatus, rj2.getStatus())
	assert.False(t, lj2.IsKilled())

	assert.Equal(t, 0, p.runningJobsCount())
}

func TestJobPool_CheckNewJobsExecutorErrorUsesTaskProjectID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		err := json.NewEncoder(w).Encode(RunnerState{
			NewJobs: []JobData{{
				Task: db.Task{
					ID:        42,
					ProjectID: 7,
				},
			}},
		})
		require.NoError(t, err)
	}))
	defer server.Close()

	previousConfig := util.Config
	util.Config = &util.ConfigType{
		WebHost: server.URL,
		Runner: &util.RunnerConfig{
			Token:      "test-token",
			Connection: &util.RunnerConnectionConfig{},
		},
	}
	defer func() {
		util.Config = previousConfig
	}()

	p := &JobPool{
		runningJobs: make(map[int]*runningJob),
		queue:       make([]*job, 0),
		startedAt:   time.Now(),
		client:      newHTTPClient(),
	}

	require.NotPanics(t, p.checkNewJobs)
	assert.Equal(t, 0, p.queueLen())
}

// TestJobPool_ConcurrentAccess models the three actors that touch the pool
// concurrently in production: the Run loop (dequeue + addRunningJob), the poll
// goroutine (snapshot + delete + enqueue) and status readers. Run with -race to
// catch concurrent map/slice access, which otherwise aborts the process with
// "fatal error: concurrent map read and map write".
func TestJobPool_ConcurrentAccess(t *testing.T) {
	initConfig(t)

	p := NewJobPool(nil)

	const workers = 8
	const iterations = 300

	var wg sync.WaitGroup
	start := make(chan struct{})

	// Running-jobs map mutators/readers.
	for w := 0; w < workers; w++ {
		base := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < iterations; i++ {
				id := base*iterations + i
				p.addRunningJob(id, &runningJob{
					job:    &tasks.LocalExecutor{Task: db.Task{ID: id}},
					status: task_logger.TaskRunningStatus,
				})
				p.getRunningJob(id)
				p.runningJobsCount()
				p.snapshotRunningJobs()
				p.hasRunningJobs()
				p.deleteRunningJob(id)
			}
		}()
	}

	// Queue mutators/readers.
	for w := 0; w < workers; w++ {
		base := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < iterations; i++ {
				id := base*iterations + i
				p.enqueue(newTestJob(id))
				p.existsInQueue(id)
				p.queueLen()
				p.dequeue()
			}
		}()
	}

	close(start)
	wg.Wait()
}

// TestJobPool_ReusesConnections guards against the TCP connection leak from
// issue #3941: creating a new http.Client (and transport) per poll cycle left
// one ESTABLISHED connection behind per request until ephemeral ports ran out.
// With the shared client, many poll cycles must reuse a single connection.
func TestJobPool_ReusesConnections(t *testing.T) {
	prevCfg := util.Config
	t.Cleanup(func() { util.Config = prevCfg })

	var newConns int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("{}"))
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			atomic.AddInt32(&newConns, 1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)

	util.Config = &util.ConfigType{
		WebHost: srv.URL,
		Runner: &util.RunnerConfig{
			Token:      "test-token",
			Executor:   &util.ExecutorConfig{},
			Connection: &util.RunnerConnectionConfig{},
		},
	}

	p := NewJobPool(nil)

	for i := 0; i < 10; i++ {
		require.True(t, p.sendProgress())
		p.checkNewJobs()
	}

	assert.Equal(t, int32(1), atomic.LoadInt32(&newConns))
}

// TestJobPool_ResolvedInventorySentOnce guards against the resend bug: a
// resolved inventory report costs the server a full snapshot replace (up to
// 20000 hosts, delete+insert across four tables), so it must be sent exactly
// once and dropped from later poll cycles once the server has acknowledged
// the PUT, the same way log records are trimmed on ack.
func TestJobPool_ResolvedInventorySentOnce(t *testing.T) {
	prevCfg := util.Config
	t.Cleanup(func() { util.Config = prevCfg })

	var mu sync.Mutex
	var received []JobProgress

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body RunnerProgress
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))

		mu.Lock()
		received = append(received, body.Jobs...)
		mu.Unlock()

		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	util.Config = &util.ConfigType{
		WebHost: srv.URL,
		Runner: &util.RunnerConfig{
			Token:      "test-token",
			Executor:   &util.ExecutorConfig{},
			Connection: &util.RunnerConnectionConfig{},
		},
	}

	p := NewJobPool(nil)

	lj := &tasks.LocalExecutor{Task: db.Task{ID: 1}}
	rj := &runningJob{job: lj, status: task_logger.TaskRunningStatus}
	lj.Logger = rj
	rj.SetResolvedInventory([]string{"web01", "db01"}, map[string][]string{"web": {"web01"}})
	p.addRunningJob(1, rj)

	require.True(t, p.sendProgress())
	require.True(t, p.sendProgress())
	require.True(t, p.sendProgress())

	require.Len(t, received, 3)
	require.NotNil(t, received[0].ResolvedInventory, "the first report must carry the resolved inventory")
	assert.Nil(t, received[1].ResolvedInventory, "the server already acknowledged it, must not be resent")
	assert.Nil(t, received[2].ResolvedInventory, "must stay unset on subsequent polls")
}

func TestJobPool_checkNewJobs_ExecutorErrorWithoutCacheCleanProjectID(t *testing.T) {
	prevCfg := util.Config
	t.Cleanup(func() { util.Config = prevCfg })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := RunnerState{
			NewJobs: []JobData{
				{
					Task:        db.Task{ID: 1, ProjectID: 42, TemplateID: 1},
					Template:    db.Template{ID: 1, App: db.AppAnsible},
					Inventory:   db.Inventory{ID: 1},
					Repository:  db.Repository{ID: 1},
					Environment: db.Environment{ID: 1},
				},
			},
			AccessKeys: map[int]db.AccessKey{},
		}
		require.NoError(t, json.NewEncoder(w).Encode(state))
	}))
	t.Cleanup(srv.Close)

	util.Config = &util.ConfigType{
		WebHost: srv.URL,
		Runner: &util.RunnerConfig{
			Token:      "test-token",
			Executor:   &util.ExecutorConfig{},
			Connection: &util.RunnerConnectionConfig{},
		},
	}

	p := NewJobPool(nil)
	p.provider = nil // simulate OSS k8s/docker stub or failed provider init

	require.NotPanics(t, func() {
		p.checkNewJobs()
	})
	assert.Equal(t, 0, p.queueLen())
}

func TestJobCheckDue(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		now      time.Time
		deadline time.Time
		interval time.Duration
		due      bool
		next     time.Time
	}{
		{
			name: "the zero deadline fires at once",
			now:  base, deadline: time.Time{}, interval: time.Second,
			due: true, next: base.Add(time.Second),
		},
		{
			name: "on the deadline",
			now:  base.Add(time.Second), deadline: base.Add(time.Second), interval: time.Second,
			due: true, next: base.Add(2 * time.Second),
		},
		{
			name: "a tick a little early still fires and keeps the grid",
			now:  base.Add(900 * time.Millisecond), deadline: base.Add(time.Second), interval: time.Second,
			due: true, next: base.Add(2 * time.Second),
		},
		{
			name: "a tick more than the tolerance early does not fire",
			now:  base.Add(400 * time.Millisecond), deadline: base.Add(time.Second), interval: time.Second,
			due: false, next: base.Add(time.Second),
		},
		{
			name: "a late tick does not push the next deadline out",
			now:  base.Add(1200 * time.Millisecond), deadline: base.Add(time.Second), interval: time.Second,
			due: true, next: base.Add(2 * time.Second),
		},
		{
			name: "a stall longer than the interval re-anchors instead of replaying",
			now:  base.Add(time.Minute), deadline: base.Add(time.Second), interval: time.Second,
			due: true, next: base.Add(time.Minute + time.Second),
		},
		{
			name: "a long interval is not fired by an ordinary tick",
			now:  base.Add(time.Second), deadline: base.Add(5 * time.Second), interval: 5 * time.Second,
			due: false, next: base.Add(5 * time.Second),
		},
		{
			name: "a long interval fires on the tick nearest its deadline",
			now:  base.Add(4700 * time.Millisecond), deadline: base.Add(5 * time.Second), interval: 5 * time.Second,
			due: true, next: base.Add(10 * time.Second),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			due, next := jobCheckDue(tt.now, tt.deadline, tt.interval)

			assert.Equal(t, tt.due, due)
			assert.Equal(t, tt.next, next)
		})
	}
}

// The job check runs on the progress ticker, so at the default interval the two
// share a period and every tick owes exactly one check. The tick does not
// arrive on an exact second: the progress report before it takes a varying
// amount of time, so the moment the decision is taken drifts back and forth.
// A rule measuring the time elapsed since the previous check loses a check
// whenever that delay shrinks, which is about every second tick.
func TestJobCheckDueNeverSkipsAtTheDefaultInterval(t *testing.T) {
	const ticks = 500
	const interval = time.Second
	// The progress report round trip of a healthy runner, so the largest
	// delay between a tick and the decision that follows it.
	const maxDelay = 300 * time.Millisecond

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	delays := map[string]func(i int) time.Duration{
		"no delay":       func(int) time.Duration { return 0 },
		"constant delay": func(int) time.Duration { return maxDelay },
		"alternating extremes": func(i int) time.Duration {
			if i%2 == 0 {
				return maxDelay
			}
			return 0
		},
		"sawtooth": func(i int) time.Duration {
			return time.Duration(i%7) * maxDelay / 6
		},
		"pseudorandom": func(i int) time.Duration {
			return time.Duration((i*2654435761)%int(maxDelay+1)) * time.Nanosecond
		},
	}

	for name, delay := range delays {
		t.Run(name, func(t *testing.T) {
			deadline := time.Time{}
			checks := 0

			for i := range ticks {
				now := base.Add(time.Duration(i)*interval + delay(i))

				due, next := jobCheckDue(now, deadline, interval)
				deadline = next

				if due {
					checks++
				}
			}

			assert.Equal(t, ticks, checks, "every tick must fire a job check")
		})
	}
}
