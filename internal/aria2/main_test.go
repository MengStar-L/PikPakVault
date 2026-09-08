package aria2

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	dir, e := os.MkdirTemp("", "vault-aria2-tests-")
	if e != nil {
		panic(e)
	}
	if os.Getenv("VAULT_ARIA2_CRASH_HELPER") != "1" {
		os.Setenv("VAULT_PROGRAM_DIR", dir)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
