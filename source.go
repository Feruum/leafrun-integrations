// Package integrations acquires project trees from external source formats.
// Its trees use io/fs and do not depend on a document engine.
package integrations

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
)

// Source opens an independent project tree. The caller must close a successful tree.
type Source interface {
	Open(context.Context) (SourceTree, error)
}

// SourceTree owns its acquired resources until Close is called. Close is
// idempotent, closes outstanding files, and releases only operation-owned data.
// FS uses slash-separated io/fs paths rooted at the selected project directory.
type SourceTree struct {
	FS     fs.FS
	Origin SourceOrigin
	Close  func() error
	// ProtectedPaths contains original native input paths for output guards.
	ProtectedPaths []string
	// NativeProtection holds frozen identities of original native inputs. These
	// are publication guards, not mirrors of FS or content validation inputs.
	NativeProtection []NativeProtection
}

// NativeProtection protects a canonical absolute original path and its acquired
// native identity. Directory guards cover the whole directory; regular file
// identities additionally protect hardlink aliases after replacement or removal.
// Info comes from an opened handle and stays usable after that handle is closed.
type NativeProtection struct {
	Path string
	Info fs.FileInfo
}

// SourceOrigin records provenance separately from the project's content digest.
type SourceOrigin struct {
	Kind          string
	Location      string
	SourceRoot    string
	Ref           string
	Revision      string
	ArchiveSHA256 string
}

// Limits bounds regular file count and expanded bytes. Zero fields use defaults.
// TAR.GZ total bytes also include tar headers, metadata, padding, and trailing bytes.
type Limits struct {
	MaxFiles      int64
	MaxTotalBytes int64
	MaxFileBytes  int64
}

const (
	DefaultMaxFiles      int64 = 10_000
	DefaultMaxTotalBytes int64 = 512 << 20
	DefaultMaxFileBytes  int64 = 128 << 20
)

var (
	ErrOptions          = errors.New("invalid source options")
	ErrArchive          = errors.New("invalid archive")
	ErrUnsafePath       = errors.New("unsafe project path")
	ErrUnsupportedEntry = errors.New("unsupported source entry")
	ErrLimit            = errors.New("source limit exceeded")
	ErrSourceRoot       = errors.New("invalid source root")
	ErrCleanup          = errors.New("source cleanup failed")
)

// ErrorKind identifies the category while SourceError.Unwrap retains its cause.
type ErrorKind string

const (
	ErrorOptions          ErrorKind = "options"
	ErrorArchive          ErrorKind = "archive"
	ErrorUnsafePath       ErrorKind = "unsafe_path"
	ErrorUnsupportedEntry ErrorKind = "unsupported_entry"
	ErrorLimit            ErrorKind = "limit"
	ErrorSourceRoot       ErrorKind = "source_root"
	ErrorCleanup          ErrorKind = "cleanup"
	ErrorInput            ErrorKind = "input"
)

// SourceError describes the failed operation. errors.Is recognizes both category
// sentinels and wrapped causes such as cancellation, checksum, or filesystem errors.
type SourceError struct {
	Kind ErrorKind
	Op   string
	Path string
	Err  error
}

func (e *SourceError) Error() string {
	return fmt.Sprintf("source %s %q: %s: %v", e.Op, e.Path, e.Kind, e.Err)
}

func (e *SourceError) Unwrap() error { return e.Err }

func (e *SourceError) Is(target error) bool {
	switch e.Kind {
	case ErrorOptions:
		return target == ErrOptions
	case ErrorArchive:
		return target == ErrArchive
	case ErrorUnsafePath:
		return target == ErrUnsafePath
	case ErrorUnsupportedEntry:
		return target == ErrUnsupportedEntry
	case ErrorLimit:
		return target == ErrLimit
	case ErrorSourceRoot:
		return target == ErrSourceRoot
	case ErrorCleanup:
		return target == ErrCleanup
	}
	return false
}
