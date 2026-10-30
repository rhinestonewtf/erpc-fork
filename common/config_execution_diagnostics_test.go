package common

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func decodeStrict(t *testing.T, doc string) (*Config, error) {
	t.Helper()
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader([]byte(doc)))
	dec.KnownFields(true)
	return &cfg, dec.Decode(&cfg)
}

func TestExecutionDiagnosticsConfig_Parse(t *testing.T) {
	cfg, err := decodeStrict(t, `
executionDiagnostics:
  enabled: false
  maxEventsPerSecond: 2.5
  burst: 20
  maxAttemptEventsPerRequest: 4
`)
	require.NoError(t, err)
	d := cfg.ExecutionDiagnostics
	require.NotNil(t, d)
	assert.False(t, d.IsEnabled())
	assert.Equal(t, 2.5, d.MaxEventsPerSecond)
	assert.Equal(t, 20, d.Burst)
	assert.Equal(t, 4, d.MaxAttemptEventsPerRequest)
	assert.NoError(t, d.Validate())

	_, err = decodeStrict(t, `
executionDiagnostics:
  sampleRate: 1
`)
	assert.Error(t, err, "unknown keys must be rejected")
}

func TestExecutionDiagnosticsConfig_DefaultsWhenOmitted(t *testing.T) {
	cfg := &Config{}
	require.NoError(t, cfg.SetDefaults(&DefaultOptions{}))
	d := cfg.ExecutionDiagnostics
	require.NotNil(t, d)
	assert.True(t, d.IsEnabled())
	assert.Equal(t, float64(DefaultExecutionDiagnosticsMaxEventsPerSecond), d.MaxEventsPerSecond)
	assert.Equal(t, DefaultExecutionDiagnosticsBurst, d.Burst)
	assert.Equal(t, DefaultExecutionDiagnosticsMaxAttemptEventsPerRequest, d.MaxAttemptEventsPerRequest)
	assert.NoError(t, d.Validate())

	var nilCfg *ExecutionDiagnosticsConfig
	assert.True(t, nilCfg.IsEnabled())

	// A larger attempt cap raises the defaulted burst so a full request fits.
	big := &ExecutionDiagnosticsConfig{MaxAttemptEventsPerRequest: 200}
	big.SetDefaults()
	assert.Equal(t, 201, big.Burst)
	assert.NoError(t, big.Validate())
}

func TestExecutionDiagnosticsConfig_ZeroMeansDefault(t *testing.T) {
	cfg, err := decodeStrict(t, `
executionDiagnostics:
  maxEventsPerSecond: 0
  burst: 0
  maxAttemptEventsPerRequest: 0
`)
	require.NoError(t, err)
	d := cfg.ExecutionDiagnostics
	require.NotNil(t, d)
	d.SetDefaults()
	// The documented defaults, not the constants: the docs promise these values.
	assert.Equal(t, 10.0, d.MaxEventsPerSecond)
	assert.Equal(t, 100, d.Burst)
	assert.Equal(t, 8, d.MaxAttemptEventsPerRequest)
	assert.NoError(t, d.Validate())
}

func TestExecutionDiagnosticsConfig_Validate(t *testing.T) {
	cases := []struct {
		name string
		cfg  ExecutionDiagnosticsConfig
	}{
		{"NonPositiveRate", ExecutionDiagnosticsConfig{MaxEventsPerSecond: -1, Burst: 10, MaxAttemptEventsPerRequest: 1}},
		{"ZeroAttempts", ExecutionDiagnosticsConfig{MaxEventsPerSecond: 1, Burst: 10, MaxAttemptEventsPerRequest: -1}},
		{"BurstTooSmall", ExecutionDiagnosticsConfig{MaxEventsPerSecond: 1, Burst: 8, MaxAttemptEventsPerRequest: 8}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Error(t, tc.cfg.Validate())
		})
	}
}
