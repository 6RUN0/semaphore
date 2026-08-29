package tasks

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A single Ansible line can be huge (a failed task echoes the whole rendered
// variable set), and bufio's own default gives up at 64 KB. The scanner is
// given a 10 MB buffer for that reason, and the "output exceeds the maximum
// allowed size of 10MB" message promises it, so the size is part of the
// contract rather than an implementation detail.
func TestLogPipeAcceptsLongLines(t *testing.T) {
	runner := &TaskRunner{pool: &TaskPool{logger: make(chan logRecord, 8)}}
	line := strings.Repeat("x", 1024*1024)

	// logPipe returns once its own reader goroutine is done, so everything it
	// accepted is already in the channel below.
	runner.logPipe(strings.NewReader(line + "\nshort\n"))

	require.Len(t, runner.pool.logger, 2, "a line under the limit must not be dropped")
	first := <-runner.pool.logger
	second := <-runner.pool.logger

	assert.Equal(t, line, first.output)
	assert.Equal(t, "short", second.output)
}
