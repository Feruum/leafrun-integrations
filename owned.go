package integrations

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ownedWorkspace uses a parent handle for allocation and removal. The explicitly
// selected temporary parent is resolved before allocation, including aliases.
type ownedWorkspace struct {
	parent      *os.Root
	root        *os.Root
	name        string
	mu          sync.Mutex
	closed      bool
	closeErr    error
	filesystems []*ownedFS
}

func newOwnedWorkspace(ctx context.Context, tempDir string) (*ownedWorkspace, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if tempDir == "" {
		tempDir = os.TempDir()
	}
	absolute, err := filepath.Abs(tempDir)
	if err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, err
	}
	parent, err := os.OpenRoot(resolved)
	if err != nil {
		return nil, err
	}
	w := &ownedWorkspace{parent: parent}
	for range 10 {
		var random [16]byte
		if _, err = rand.Read(random[:]); err != nil {
			break
		}
		name := "leafrun-source-" + hex.EncodeToString(random[:])
		err = parent.Mkdir(name, 0700)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			break
		}
		w.name = name
		w.root, err = parent.OpenRoot(name)
		if err == nil {
			err = ctx.Err()
		}
		if err == nil {
			return w, nil
		}
		break
	}
	return nil, errors.Join(err, w.close())
}

func (w *ownedWorkspace) tree(name string) (fs.FS, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil, fs.ErrClosed
	}
	root, err := w.root.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	owned := &ownedFS{root: root, files: make(map[*ownedFile]struct{})}
	w.filesystems = append(w.filesystems, owned)
	return owned, nil
}

func (w *ownedWorkspace) close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return w.closeErr
	}
	w.closed = true
	var errs []error
	for _, filesystem := range w.filesystems {
		errs = append(errs, filesystem.close())
	}
	if w.root != nil {
		errs = append(errs, w.root.Close())
	}
	if w.name != "" {
		errs = append(errs, w.parent.RemoveAll(w.name))
	}
	errs = append(errs, w.parent.Close())
	if cause := errors.Join(errs...); cause != nil {
		w.closeErr = &SourceError{Kind: ErrorCleanup, Op: "close", Err: cause}
	}
	return w.closeErr
}

// ownedFS revokes outstanding files before removal, including on Windows where
// a retained file handle can otherwise prevent directory cleanup.
type ownedFS struct {
	root   *os.Root
	mu     sync.Mutex
	closed bool
	files  map[*ownedFile]struct{}
}

func (f *ownedFS) Open(name string) (fs.File, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrClosed}
	}
	if !fs.ValidPath(name) || strings.Contains(name, "\\") {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	file, err := f.root.Open(name)
	if err != nil {
		return nil, err
	}
	wrapped := &ownedFile{file: file, owner: f}
	f.files[wrapped] = struct{}{}
	return wrapped, nil
}

func (f *ownedFS) close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	var errs []error
	for file := range f.files {
		file.closed = true
		errs = append(errs, file.file.Close())
	}
	f.files = nil
	errs = append(errs, f.root.Close())
	return errors.Join(errs...)
}

type ownedFile struct {
	file   *os.File
	owner  *ownedFS
	closed bool
}

func (f *ownedFile) Read(data []byte) (int, error) {
	f.owner.mu.Lock()
	defer f.owner.mu.Unlock()
	if f.closed {
		return 0, fs.ErrClosed
	}
	return f.file.Read(data)
}

func (f *ownedFile) Stat() (fs.FileInfo, error) {
	f.owner.mu.Lock()
	defer f.owner.mu.Unlock()
	if f.closed {
		return nil, fs.ErrClosed
	}
	return f.file.Stat()
}

func (f *ownedFile) ReadDir(n int) ([]fs.DirEntry, error) {
	f.owner.mu.Lock()
	defer f.owner.mu.Unlock()
	if f.closed {
		return nil, fs.ErrClosed
	}
	return f.file.ReadDir(n)
}

func (f *ownedFile) Seek(offset int64, whence int) (int64, error) {
	f.owner.mu.Lock()
	defer f.owner.mu.Unlock()
	if f.closed {
		return 0, fs.ErrClosed
	}
	return f.file.Seek(offset, whence)
}

func (f *ownedFile) Close() error {
	f.owner.mu.Lock()
	defer f.owner.mu.Unlock()
	if f.closed {
		return fs.ErrClosed
	}
	f.closed = true
	delete(f.owner.files, f)
	return f.file.Close()
}
