package apiserve

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sipeed/picoclaw/pkg/config"
)

// An explicit home must pin the config file. Resolving the path from the
// environment instead would make --home load ~/.picoclaw/config.json while
// writing the database into the requested home — the server then fails to start
// with "model not found in model_list" even though the requested home holds a
// perfectly good config.json.
func TestResolveConfigPathFor_TakesTheGivenHomeOverTheEnvironment(t *testing.T) {
	envHome := t.TempDir()
	t.Setenv(config.EnvConfig, "")
	t.Setenv(config.EnvHome, envHome)

	wanted := t.TempDir()
	got := ResolveConfigPathFor(wanted)

	require.Equal(t, filepath.Join(wanted, "config.json"), got)
	require.NotEqual(t, filepath.Join(envHome, "config.json"), got,
		"the given home must win over PICOCLAW_HOME")
}

// PICOCLAW_CONFIG still overrides everything, including an explicit home.
func TestResolveConfigPathFor_HonoursExplicitConfigEnv(t *testing.T) {
	explicit := filepath.Join(t.TempDir(), "custom.json")
	t.Setenv(config.EnvConfig, explicit)

	require.Equal(t, explicit, ResolveConfigPathFor(t.TempDir()))
}

func TestResolveConfigPath_FallsBackToTheEnvironmentHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv(config.EnvConfig, "")
	t.Setenv(config.EnvHome, home)

	require.Equal(t, filepath.Join(home, "config.json"), ResolveConfigPath())
}
