package db

import (
	"testing"
	"time"

	"github.com/semaphoreui/semaphore/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func resetConfig(maxTTL string) {
	util.Config = &util.ConfigType{
		JWT: &util.JWTConfig{
			MaxTTL: maxTTL,
		},
	}
}

func TestTemplateJWTParams_Validate(t *testing.T) {
	resetConfig("1h")

	tests := []struct {
		name    string
		params  *TemplateJWTParams
		wantErr string
	}{
		{
			"nil", nil, "",
		},
		{
			"disabled",
			&TemplateJWTParams{
				Enabled: false,
				TTL:     "999h",
			},
			"",
		},
		{
			"ok",
			&TemplateJWTParams{
				Enabled:  true,
				Audience: []string{"a"},
				TTL:      "30m",
			},
			"",
		},
		{
			"empty audience entry",
			&TemplateJWTParams{
				Enabled:  true,
				Audience: []string{""},
			},
			"audience entries must not be empty",
		},
		{
			"invalid ttl",
			&TemplateJWTParams{
				Enabled: true,
				TTL:     "nope",
			},
			"invalid JWT TTL",
		},
		{
			"non-positive ttl",
			&TemplateJWTParams{
				Enabled: true,
				TTL:     "0s",
			},
			"must be positive",
		},
		{
			"ttl exceeds max",
			&TemplateJWTParams{
				Enabled: true,
				TTL:     "2h",
			},
			"exceeds configured maximum",
		},
		// The maximum is inclusive: a template configured at exactly the limit
		// must keep working when the limit is what it was chosen to match.
		{
			"ttl equal to max",
			&TemplateJWTParams{
				Enabled: true,
				TTL:     "1h",
			},
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.params.Validate()
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			assert.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestTemplateJWTParams_Validate_AudienceCap(t *testing.T) {
	resetConfig("1h")

	audience := func(n int) []string {
		aud := make([]string, n)
		for i := range aud {
			aud[i] = "x"
		}
		return aud
	}

	over := &TemplateJWTParams{Enabled: true, Audience: audience(maxJWTAudienceEntries + 1)}
	assert.ErrorContains(t, over.Validate(), "at most")

	// The cap is the largest accepted size, not the first rejected one.
	atCap := &TemplateJWTParams{Enabled: true, Audience: audience(maxJWTAudienceEntries)}
	assert.NoError(t, atCap.Validate())
}

// The TTL cap is read from the server config on every validation, so each way
// of leaving it unusable has to resolve to the 24h default rather than to
// "no maximum".
func TestTemplateJWTParams_Validate_MaxTTLFallbacks(t *testing.T) {
	orig := util.Config
	defer func() { util.Config = orig }()

	huge := &TemplateJWTParams{Enabled: true, TTL: "999h"}

	tests := []struct {
		name    string
		maxTTL  string
		wantErr string
	}{
		{"unset falls back to 24h", "", "exceeds configured maximum"},
		{"zero falls back to 24h", "0s", "exceeds configured maximum"},
		{"negative falls back to 24h", "-1h", "exceeds configured maximum"},
		{"unparsable falls back to 24h", "nonsense", "exceeds configured maximum"},
		{"explicit maximum above the ttl", "1000h", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetConfig(tt.maxTTL)

			err := huge.Validate()

			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			assert.ErrorContains(t, err, tt.wantErr)
		})
	}

	// Without a config there is no maximum to enforce: a template must not be
	// rejected against a limit that was never configured.
	t.Run("no config means no maximum", func(t *testing.T) {
		util.Config = nil

		assert.NoError(t, huge.Validate())
	})
}

// ParsedTTL is called on a template's params, which are nil whenever the
// feature was never configured.
func TestTemplateJWTParams_ParsedTTL(t *testing.T) {
	t.Run("nil params", func(t *testing.T) {
		d, err := (*TemplateJWTParams)(nil).ParsedTTL()

		require.NoError(t, err)
		assert.Zero(t, d)
	})

	t.Run("unset ttl", func(t *testing.T) {
		d, err := (&TemplateJWTParams{Enabled: true}).ParsedTTL()

		require.NoError(t, err)
		assert.Zero(t, d)
	})

	t.Run("parsed", func(t *testing.T) {
		d, err := (&TemplateJWTParams{TTL: "90m"}).ParsedTTL()

		require.NoError(t, err)
		assert.Equal(t, 90*time.Minute, d)
	})

	t.Run("unparsable", func(t *testing.T) {
		_, err := (&TemplateJWTParams{TTL: "later"}).ParsedTTL()

		assert.ErrorContains(t, err, "invalid JWT TTL")
	})
}

func TestTemplateJWTParams_ScanValue(t *testing.T) {
	original := &TemplateJWTParams{
		Enabled:  true,
		Audience: []string{"a", "b"},
		TTL:      "30m",
	}

	v, err := original.Value()
	require.NoError(t, err)
	require.NotNil(t, v)

	var restored TemplateJWTParams
	require.NoError(t, restored.Scan(v))
	assert.Equal(t, *original, restored)

	var zero TemplateJWTParams
	require.NoError(t, zero.Scan(nil))
	assert.Equal(t, TemplateJWTParams{}, zero)
}
