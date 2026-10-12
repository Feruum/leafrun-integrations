//go:build !windows

package integrations

import (
	"os"
	"testing"
)

func restrictTestPrivateKey(t *testing.T, name string) {
	t.Helper()
	if err := os.Chmod(name, 0600); err != nil {
		t.Fatal(err)
	}
}
