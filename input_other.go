//go:build !unix

package integrations

import "os"

func openNativeArchiveInput(name string) (*os.File, error) {
	return os.Open(name)
}
