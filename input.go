package integrations

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// openArchiveInput validates the handle that will be copied, rather than a path
// that may be replaced between validation and open. Unix acquisition itself is
// nonblocking so a substituted FIFO cannot prevent cancellation or cleanup.
func openArchiveInput(ctx context.Context, name string) (input *os.File, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	opened, err := openNativeArchiveInput(name)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, opened.Close())
			input = nil
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := opened.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("archive input is not a regular file: %w", fs.ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return opened, nil
}
