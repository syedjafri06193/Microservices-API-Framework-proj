package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type sample struct {
	Name       string        `env:"TEST_NAME,required"`
	Env        string        `env:"TEST_ENV" envDefault:"dev" validate:"oneof=dev staging prod"`
	Ratio      float64       `env:"TEST_RATIO" envDefault:"0.1" validate:"gte=0,lte=1"`
	Count      int           `env:"TEST_COUNT" envDefault:"3"`
	Enabled    bool          `env:"TEST_ENABLED" envDefault:"true"`
	Timeout    time.Duration `env:"TEST_TIMEOUT" envDefault:"5s"`
	Peers      []string      `env:"TEST_PEERS"`
	unexported string
}

func TestDefaultsApplyWhenUnset(t *testing.T) {
	t.Setenv("TEST_NAME", "svc")
	var c sample
	require.NoError(t, Load(&c))
	require.Equal(t, "svc", c.Name)
	require.Equal(t, "dev", c.Env)
	require.Equal(t, 0.1, c.Ratio)
	require.Equal(t, 3, c.Count)
	require.True(t, c.Enabled)
	require.Equal(t, 5*time.Second, c.Timeout)
}

func TestRequiredFieldIsEnforced(t *testing.T) {
	var c sample
	err := Load(&c)
	require.Error(t, err)
	require.Contains(t, err.Error(), "TEST_NAME is required")
}

func TestValidationNamesTheVariableTheValueAndTheConstraint(t *testing.T) {
	// Not "invalid config". An operator should be able to fix it from the
	// log line without reading the source.
	t.Setenv("TEST_NAME", "svc")
	t.Setenv("TEST_ENV", "production")

	var c sample
	err := Load(&c)
	require.Error(t, err)
	require.Contains(t, err.Error(), "TEST_ENV")
	require.Contains(t, err.Error(), "must be one of [dev staging prod]")
	require.Contains(t, err.Error(), `got "production"`)
}

func TestEveryProblemIsReportedAtOnce(t *testing.T) {
	// An operator fixing a broken deployment should get the whole list, not
	// discover the next missing variable on each restart.
	t.Setenv("TEST_ENV", "nope")
	t.Setenv("TEST_RATIO", "5")

	var c sample
	err := Load(&c)
	require.Error(t, err)
	require.Contains(t, err.Error(), "TEST_NAME")
	require.Contains(t, err.Error(), "TEST_ENV")
	require.Contains(t, err.Error(), "TEST_RATIO")
}

func TestRangeValidation(t *testing.T) {
	t.Setenv("TEST_NAME", "svc")
	t.Setenv("TEST_RATIO", "1.5")
	var c sample
	err := Load(&c)
	require.Error(t, err)
	require.Contains(t, err.Error(), "must be at most 1")
}

func TestMalformedValuesAreRejectedWithTheirType(t *testing.T) {
	t.Setenv("TEST_NAME", "svc")
	t.Setenv("TEST_TIMEOUT", "quite a while")
	var c sample
	err := Load(&c)
	require.Error(t, err)
	require.Contains(t, err.Error(), "TEST_TIMEOUT")
	require.Contains(t, err.Error(), "duration")
}

func TestCommaSeparatedSlices(t *testing.T) {
	t.Setenv("TEST_NAME", "svc")
	t.Setenv("TEST_PEERS", "a, b ,c")
	var c sample
	require.NoError(t, Load(&c))
	require.Equal(t, []string{"a", "b", "c"}, c.Peers)
}

func TestEmptyStringCountsAsUnset(t *testing.T) {
	// A Kubernetes ConfigMap that sets a key to "" should behave the same
	// as one that omits it, or an operator clearing a value gets a
	// confusing zero instead of the default.
	t.Setenv("TEST_NAME", "svc")
	t.Setenv("TEST_ENV", "")
	var c sample
	require.NoError(t, Load(&c))
	require.Equal(t, "dev", c.Env)
}

func TestLoadRejectsNonPointers(t *testing.T) {
	require.Error(t, Load(sample{}))
	require.Error(t, Load(nil))
}
