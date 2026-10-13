package integrations

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Metadata identities are collected before ref resolution. No configuration,
// credential, object or key bytes are read by this guard collector. Git itself
// remains responsible for reading repository objects and native authentication.
func protectLocalGit(ctx context.Context, r *gitRunner, location string) ([]NativeProtection, string, error) {
	data, err := r.output(ctx, "-C", location, "rev-parse", "--path-format=absolute", "--is-bare-repository", "--git-dir", "--git-common-dir")
	if err != nil {
		return nil, "", sourceErr(ErrorInput, "local repository", location, err)
	}
	lines := strings.Split(stripGitLineTerminator(data), "\n")
	if len(lines) != 3 {
		return nil, "", sourceErr(ErrorInput, "local repository", location, errors.New("invalid native repository metadata response"))
	}
	seen := map[string]bool{}
	var guards []NativeProtection
	for _, name := range lines[1:] {
		absolute, err := filepath.Abs(name)
		if err != nil {
			return nil, "", err
		}
		resolved, err := filepath.EvalSymlinks(absolute)
		if err != nil {
			return nil, "", err
		}
		if seen[resolved] {
			continue
		}
		seen[resolved] = true
		root, err := os.OpenRoot(resolved)
		if err != nil {
			return nil, "", err
		}
		walkErr := fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if errors.Is(walkErr, fs.ErrNotExist) {
				return nil
			}
			if walkErr != nil {
				return walkErr
			}
			if entry.Type()&fs.ModeSymlink != 0 {
				return nil
			}
			info, err := root.Lstat(name)
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			if !info.IsDir() && !info.Mode().IsRegular() {
				return nil
			}
			// Stat from the actual handle eagerly freezes Windows file identity.
			file, err := root.OpenFile(name, nativeGuardOpenFlags(), 0)
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			frozen, statErr := file.Stat()
			closeErr := file.Close()
			if err := errors.Join(statErr, closeErr); err != nil {
				return err
			}
			if frozen.IsDir() || frozen.Mode().IsRegular() {
				guards = append(guards, NativeProtection{Path: filepath.Join(resolved, filepath.FromSlash(name)), Info: frozen})
			}
			return nil
		})
		if err := errors.Join(walkErr, root.Close()); err != nil {
			return nil, "", sourceErr(ErrorInput, "protect git metadata", resolved, err)
		}
	}
	var working string
	if lines[0] == "false" {
		data, err = r.output(ctx, "-C", location, "rev-parse", "--show-toplevel")
		if err != nil {
			return nil, "", err
		}
		working, err = filepath.EvalSymlinks(stripGitLineTerminator(data))
		if err != nil {
			return nil, "", err
		}
		root, err := os.OpenRoot(working)
		if err != nil {
			return nil, "", err
		}
		info, statErr := root.Lstat(".git")
		if statErr == nil && info.Mode().IsRegular() {
			file, openErr := root.OpenFile(".git", nativeGuardOpenFlags(), 0)
			if openErr != nil {
				_ = root.Close()
				return nil, "", openErr
			}
			frozen, freezeErr := file.Stat()
			closeErr := file.Close()
			if err := errors.Join(freezeErr, closeErr); err != nil {
				_ = root.Close()
				return nil, "", err
			}
			if !frozen.Mode().IsRegular() {
				_ = root.Close()
				return nil, "", errors.New("Git indirection file changed to a non-regular entry")
			}
			guards = append(guards, NativeProtection{Path: filepath.Join(working, ".git"), Info: frozen})
		} else if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
			_ = root.Close()
			return nil, "", statErr
		}
		if err := root.Close(); err != nil {
			return nil, "", err
		}
	}
	return guards, working, nil
}

// Working files are optional original-input guards. Their state never determines
// committed bytes or source validity: bare, missing and uncommitted files remain
// valid commit sources. Root confinement prevents following outside links.
func protectWorkingGitFiles(ctx context.Context, working, selected string, entries []gitEntry) []NativeProtection {
	root, err := os.OpenRoot(working)
	if err != nil {
		return nil
	}
	defer root.Close()
	var guards []NativeProtection
	for _, entry := range entries {
		if ctx.Err() != nil {
			break
		}
		if entry.directory || selected != "." && !strings.HasPrefix(entry.name, selected+"/") {
			continue
		}
		info, err := root.Lstat(entry.name)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		file, err := root.OpenFile(entry.name, nativeGuardOpenFlags(), 0)
		if err != nil {
			continue
		}
		frozen, statErr := file.Stat()
		closeErr := file.Close()
		if statErr == nil && closeErr == nil && frozen.Mode().IsRegular() {
			guards = append(guards, NativeProtection{Path: filepath.Join(working, filepath.FromSlash(entry.name)), Info: frozen})
		}
	}
	return guards
}
