package vault

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	dir, e := os.MkdirTemp("", "vault-program-tests-")
	if e != nil {
		panic(e)
	}
	os.Setenv("VAULT_PROGRAM_DIR", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
