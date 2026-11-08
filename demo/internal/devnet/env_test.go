package devnet

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestEnv_ReadsWhatDotenvExampleWrites(t *testing.T) {
	path := writeFile(t, `# --- Coinbase CDP ---
CDP_API_KEY_ID=key-id
export CDP_API_KEY_SECRET="c2VjcmV0+/=="
SOLANA_RPC_URL='https://api.devnet.solana.com'

# A commented-out override:
# CDP_BASE_URL=http://localhost:1
NOT_A_PAIR
EMPTY=
`)

	env, err := Env(path)

	require.NoError(t, err)
	assert.Equal(t, "key-id", env("CDP_API_KEY_ID"))
	assert.Equal(t, "c2VjcmV0+/==", env("CDP_API_KEY_SECRET"), "quotes stripped, base64 kept whole")
	assert.Equal(t, "https://api.devnet.solana.com", env("SOLANA_RPC_URL"))
	assert.Empty(t, env("CDP_BASE_URL"), "a commented line sets nothing")
	assert.Empty(t, env("EMPTY"))
}

func TestEnv_TheEnvironmentWinsOverTheFile(t *testing.T) {
	t.Setenv("CDP_API_KEY_ID", "from-environment")

	env, err := Env(writeFile(t, "CDP_API_KEY_ID=from-file\n"))

	require.NoError(t, err)
	assert.Equal(t, "from-environment", env("CDP_API_KEY_ID"))
}

func TestEnv_TheFirstFileWins(t *testing.T) {
	first := writeFile(t, "A=first\n")
	second := writeFile(t, "A=second\nB=second\n")

	env, err := Env(first, second)

	require.NoError(t, err)
	assert.Equal(t, "first", env("A"))
	assert.Equal(t, "second", env("B"))
}

func TestEnv_AMissingFileIsSkipped(t *testing.T) {
	env, err := Env(filepath.Join(t.TempDir(), "no-such.env"))

	require.NoError(t, err)
	assert.Empty(t, env("CLEARGATE_DEMO_UNSET_VARIABLE"))
}
