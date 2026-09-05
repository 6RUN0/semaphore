package util

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// intHoldsOverflowingInterval reports whether an int is wide enough to express a
// value past the duration limit. On 32-bit targets (386, arm) it is not, so
// those cases cannot occur there and are skipped.
const intHoldsOverflowingInterval = int64(math.MaxInt) > maxRunnerCheckIntervalSec

// A variable, not a constant: int(maxIntervalSec) would be a constant
// conversion and fail to compile on 32-bit even inside a guarded branch.
var maxIntervalSec = maxRunnerCheckIntervalSec

func TestRunnerCheckInterval(t *testing.T) {
	tests := []struct {
		name     string
		runner   *RunnerConfig
		expected time.Duration
	}{
		{"unset field", &RunnerConfig{}, time.Second},
		{"zero falls back", &RunnerConfig{CheckIntervalSeconds: 0}, time.Second},
		{"negative falls back", &RunnerConfig{CheckIntervalSeconds: -5}, time.Second},
		{"configured", &RunnerConfig{CheckIntervalSeconds: 10}, 10 * time.Second},
		{"large", &RunnerConfig{CheckIntervalSeconds: 300}, 5 * time.Minute},
	}

	if intHoldsOverflowingInterval {
		tests = append(tests,
			struct {
				name     string
				runner   *RunnerConfig
				expected time.Duration
			}{"max representable", &RunnerConfig{CheckIntervalSeconds: int(maxIntervalSec)},
				time.Duration(maxRunnerCheckIntervalSec) * time.Second},
			struct {
				name     string
				runner   *RunnerConfig
				expected time.Duration
			}{"overflows falls back", &RunnerConfig{CheckIntervalSeconds: int(maxIntervalSec) + 1},
				time.Second},
			struct {
				name     string
				runner   *RunnerConfig
				expected time.Duration
			}{"largest int", &RunnerConfig{CheckIntervalSeconds: math.MaxInt},
				time.Second},
		)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conf := &ConfigType{Runner: tt.runner}
			assert.Equal(t, tt.expected, conf.RunnerCheckInterval())
		})
	}
}

// The runner section is optional in the config file, so the accessor must not
// assume it was parsed.
func TestRunnerCheckIntervalWithoutRunnerSection(t *testing.T) {
	conf := &ConfigType{}

	assert.NotPanics(t, func() {
		assert.Equal(t, time.Second, conf.RunnerCheckInterval())
	})
}

// The result must always be safe for time.NewTicker.
func TestRunnerCheckIntervalNeverPanicsTicker(t *testing.T) {
	secs := []int{0, -1, 1, 300, math.MaxInt}
	if intHoldsOverflowingInterval {
		secs = append(secs, int(maxIntervalSec), int(maxIntervalSec)+1)
	}

	for _, sec := range secs {
		conf := &ConfigType{Runner: &RunnerConfig{CheckIntervalSeconds: sec}}
		d := conf.RunnerCheckInterval()

		assert.Positive(t, d, "interval must stay positive for CheckIntervalSeconds=%d", sec)
		assert.NotPanics(t, func() {
			time.NewTicker(d).Stop()
		}, "time.NewTicker must accept the interval for CheckIntervalSeconds=%d", sec)
	}
}

// The default tag must match the constant.
func TestRunnerCheckIntervalDefaultMatchesTag(t *testing.T) {
	conf := &ConfigType{Runner: &RunnerConfig{}}
	require.NoError(t, loadDefaultsToObject(conf))

	assert.Equal(t, defaultRunnerCheckIntervalSec, conf.Runner.CheckIntervalSeconds)
	assert.Equal(t, time.Second, conf.RunnerCheckInterval())
}

// The schema is what an editor and a CI configuration check read, so it is the
// only description of check_interval_seconds most operators ever see. It must
// not promise a cadence the runner does not honour: the field spaces out the
// job poll alone, while the progress report is fixed at one second because the
// server also uses it as the runner's heartbeat.
func TestConfigSchemaDescribesTheCheckIntervalAsCoded(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("..", "config.schema.yaml"))
	require.NoError(t, err)

	var schema struct {
		Defs struct {
			Runner struct {
				Properties struct {
					CheckInterval struct {
						Default     int    `yaml:"default"`
						Minimum     int    `yaml:"minimum"`
						Description string `yaml:"description"`
					} `yaml:"check_interval_seconds"`
				} `yaml:"properties"`
			} `yaml:"RunnerConfig"`
		} `yaml:"$defs"`
	}

	require.NoError(t, yaml.Unmarshal(content, &schema))

	field := schema.Defs.Runner.Properties.CheckInterval

	assert.Equal(t, defaultRunnerCheckIntervalSec, field.Default)
	assert.Equal(t, 1, field.Minimum)
	assert.NotEmpty(t, field.Description)
	assert.NotContains(t, field.Description, "reports progress")
}
