package integrations

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestZIPRejectsMalformedCentralExtraOutsideSourceRoot(t *testing.T) {
	for _, extra := range [][]byte{{0xaa}, {0xab, 0xcd, 0xff, 0xff}, {0xab, 0xcd, 0, 0, 0xaa}} {
		t.Run(hex.EncodeToString(extra), func(t *testing.T) {
			path, raw := writeArchive(t, "zip", []archiveRecord{{name: "selected/main.typ", data: []byte("ok")}, {name: "ignored", data: []byte("ok")}})
			central := bytes.Index(raw, []byte{'P', 'K', 1, 2})
			if central < 0 {
				t.Fatal("central header missing")
			}
			central += 46 + int(binary.LittleEndian.Uint16(raw[central+28:central+30])) + int(binary.LittleEndian.Uint16(raw[central+30:central+32])) + int(binary.LittleEndian.Uint16(raw[central+32:central+34]))
			end := len(raw) - 22
			if !bytes.Equal(raw[central:central+4], []byte{'P', 'K', 1, 2}) || binary.LittleEndian.Uint16(raw[central+30:central+32]) != 0 {
				t.Fatal("unexpected ignored central fixture")
			}
			binary.LittleEndian.PutUint16(raw[central+30:central+32], uint16(len(extra)))
			binary.LittleEndian.PutUint32(raw[end+12:end+16], binary.LittleEndian.Uint32(raw[end+12:end+16])+uint32(len(extra)))
			mutated := append([]byte(nil), raw[:end]...)
			mutated = append(mutated, extra...)
			mutated = append(mutated, raw[end:]...)
			replaceArchiveBytes(t, path, mutated)
			assertArchiveFailure(t, ArchiveSource{Path: path, SourceRoot: "selected"}, zip.ErrFormat)
		})
	}
}

func TestTARRejectsInvalidGlobalPAXNumericMetadata(t *testing.T) {
	for _, flag := range []byte{tar.TypeXHeader, tar.TypeXGlobalHeader} {
		for _, key := range []string{"mtime", "uid", "gid", "atime", "ctime"} {
			t.Run(string(flag)+"/"+key, func(t *testing.T) {
				for _, value := range []string{"invalid", "9223372036854775808"} {
					// Superseding a malformed value must not make an archive valid.
					data := append(paxRecord(key, value), paxRecord(key, "1")...)
					raw := tarRecord(t, "Metadata.0", flag, data)
					raw = append(raw, tarRecord(t, "main.typ", tar.TypeReg, []byte("ok"))...)
					path := gzipTAR(t, append(raw, make([]byte, 1024)...))
					assertArchiveFailure(t, ArchiveSource{Path: path}, ErrArchive)
				}
			})
		}
	}
}

func TestTARAcceptsValidNumericPAXMetadata(t *testing.T) {
	for _, flag := range []byte{tar.TypeXHeader, tar.TypeXGlobalHeader} {
		data := append(paxRecord("uid", "42"), paxRecord("gid", "7")...)
		data = append(data, paxRecord("mtime", "-123.123456789012")...)
		data = append(data, paxRecord("atime", "123.000000001")...)
		data = append(data, paxRecord("ctime", "0")...)
		raw := tarRecord(t, "Metadata.0", flag, data)
		raw = append(raw, tarRecord(t, "main.typ", tar.TypeReg, []byte("ok"))...)
		path := gzipTAR(t, append(raw, make([]byte, 1024)...))
		tree, err := (ArchiveSource{Path: path}).Open(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		read, readErr := fs.ReadFile(tree.FS, "main.typ")
		closeErr := tree.Close()
		if readErr != nil || closeErr != nil || string(read) != "ok" {
			t.Fatalf("valid metadata: %q %v %v", read, readErr, closeErr)
		}
	}
}

func TestArchiveProvenanceResolvesSelectedAlias(t *testing.T) {
	for _, kind := range []string{"zip", "tar.gz"} {
		path, raw := writeArchive(t, kind, []archiveRecord{{name: "project/main.typ", data: []byte("accepted")}})
		alias := filepath.Join(t.TempDir(), "selected."+kind)
		if err := os.Symlink(path, alias); err != nil {
			t.Skipf("file symlinks unavailable: %v", err)
		}
		canonical := canonicalArchivePath(t, path)
		tree, err := (ArchiveSource{Path: alias, SourceRoot: "project", Options: ArchiveOptions{TempDir: t.TempDir()}}).Open(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := tree.Close(); err != nil {
				t.Error(err)
			}
		})
		if tree.Origin.Location != canonical || len(tree.ProtectedPaths) != 1 || tree.ProtectedPaths[0] != canonical {
			t.Fatalf("selected alias provenance: %+v %v", tree.Origin, tree.ProtectedPaths)
		}
		sha := sha256.Sum256(raw)
		if tree.Origin.ArchiveSHA256 != hex.EncodeToString(sha[:]) {
			t.Fatalf("selected alias digest: %+v", tree.Origin)
		}
		if err := os.Remove(alias); err != nil {
			t.Fatal(err)
		}
		replacement, _ := writeArchive(t, kind, []archiveRecord{{name: "project/main.typ", data: []byte("replacement")}})
		if err := os.Symlink(replacement, alias); err != nil {
			t.Fatal(err)
		}
		read, err := fs.ReadFile(tree.FS, "main.typ")
		if err != nil || string(read) != "accepted" {
			t.Fatalf("retargeted archive alias changed tree: %q %v", read, err)
		}
		if err := tree.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("original archive deleted: %v", err)
		}
		if _, err := os.Stat(replacement); err != nil {
			t.Fatalf("replacement archive deleted: %v", err)
		}
	}
}

func canonicalArchivePath(t *testing.T, name string) string {
	t.Helper()
	absolute, err := filepath.Abs(name)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

func TestTARHonorsGlobalPAXPathFromStandardWriter(t *testing.T) {
	var raw bytes.Buffer
	w := tar.NewWriter(&raw)
	if err := w.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"path": "renamed.typ"}, Format: tar.FormatPAX}); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteHeader(&tar.Header{Name: "main.typ", Typeflag: tar.TypeReg, Mode: 0600, Size: 2, Format: tar.FormatUSTAR}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	path := gzipTAR(t, raw.Bytes())
	assertTARContents(t, path, map[string]string{"renamed.typ": "ok"})
}

func TestTARHonorsGlobalPAXSize(t *testing.T) {
	raw := tarRecord(t, "GlobalHead.0", tar.TypeXGlobalHeader, paxRecord("size", "2"))
	raw = append(raw, sizedTARRecord(t, "main.typ", "ok", 1)...)
	path := gzipTAR(t, append(raw, make([]byte, 1024)...))
	assertTARContents(t, path, map[string]string{"main.typ": "ok"})
}

func TestTARGlobalPAXSizeMovesFollowingHeaderBoundary(t *testing.T) {
	contents := strings.Repeat("x", 513)
	raw := tarRecord(t, "GlobalHead.0", tar.TypeXGlobalHeader, paxRecord("size", "513"))
	raw = append(raw, sizedTARRecord(t, "large.typ", contents, 1)...)
	raw = append(raw, tarRecord(t, "GlobalHead.1", tar.TypeXGlobalHeader, paxRecord("size", ""))...)
	raw = append(raw, tarRecord(t, "following.typ", tar.TypeReg, []byte("ok"))...)
	path := gzipTAR(t, append(raw, make([]byte, 1024)...))
	assertTARContents(t, path, map[string]string{"large.typ": contents, "following.typ": "ok"})
}

func TestTARGlobalPAXDeletionFromStandardWriter(t *testing.T) {
	var raw bytes.Buffer
	w := tar.NewWriter(&raw)
	for _, value := range []string{"renamed.typ", ""} {
		if err := w.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"path": value}, Format: tar.FormatPAX}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.WriteHeader(&tar.Header{Name: "main.typ", Typeflag: tar.TypeReg, Mode: 0600, Size: 2, Format: tar.FormatUSTAR}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	path := gzipTAR(t, raw.Bytes())
	assertTARContents(t, path, map[string]string{"main.typ": "ok"})
}

func sizedTARRecord(t *testing.T, name, data string, baseSize int64) []byte {
	t.Helper()
	raw := tarRecord(t, name, tar.TypeReg, []byte(data))
	copy(raw[124:136], fmt.Sprintf("%011o\x00", baseSize))
	fixTarChecksum(raw[:512])
	return raw
}

func assertTARContents(t *testing.T, path string, contents map[string]string) {
	t.Helper()
	tree, err := (ArchiveSource{Path: path}).Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := tree.Close(); err != nil {
			t.Error(err)
		}
	})
	for name, want := range contents {
		data, err := fs.ReadFile(tree.FS, name)
		if err != nil || string(data) != want {
			t.Fatalf("PAX effective %q: %q %v; want %q", name, data, err, want)
		}
	}
	var files int
	if err := fs.WalkDir(tree.FS, ".", func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			files++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if files != len(contents) {
		t.Fatalf("unexpected PAX files: %d; want %d", files, len(contents))
	}
}

func TestTARHonorsGlobalLocalAndBasePAXPrecedence(t *testing.T) {
	global := append(paxRecord("path", "global/main.typ"), paxRecord("size", "2")...)
	local := append(paxRecord("path", "local/main.typ"), paxRecord("size", "3")...)
	clear := append(paxRecord("path", ""), paxRecord("size", "")...)
	raw := tarRecord(t, "GlobalHead.0", tar.TypeXGlobalHeader, global)
	raw = append(raw, tarRecord(t, "PaxHead.0", tar.TypeXHeader, local)...)
	raw = append(raw, sizedTARRecord(t, "ignored-first.typ", "abc", 1)...)
	raw = append(raw, sizedTARRecord(t, "ignored-second.typ", "ok", 1)...)
	raw = append(raw, tarRecord(t, "GlobalHead.1", tar.TypeXGlobalHeader, clear)...)
	raw = append(raw, sizedTARRecord(t, "base/main.typ", "z", 1)...)
	path := gzipTAR(t, append(raw, make([]byte, 1024)...))
	assertTARContents(t, path, map[string]string{"local/main.typ": "abc", "global/main.typ": "ok", "base/main.typ": "z"})
}

func TestTAREmptyLocalPAXSuppressesGlobalForOneFile(t *testing.T) {
	global := append(paxRecord("path", "global.typ"), paxRecord("size", "2")...)
	clear := append(paxRecord("path", ""), paxRecord("size", "")...)
	raw := tarRecord(t, "GlobalHead.0", tar.TypeXGlobalHeader, global)
	raw = append(raw, tarRecord(t, "PaxHead.0", tar.TypeXHeader, clear)...)
	raw = append(raw, sizedTARRecord(t, "base.typ", "a", 1)...)
	raw = append(raw, sizedTARRecord(t, "ignored.typ", "ok", 1)...)
	path := gzipTAR(t, append(raw, make([]byte, 1024)...))
	assertTARContents(t, path, map[string]string{"base.typ": "a", "global.typ": "ok"})
}

func TestTARChecksEffectiveGlobalPAXSizeAndDuplicateNames(t *testing.T) {
	raw := tarRecord(t, "GlobalHead.0", tar.TypeXGlobalHeader, paxRecord("size", "2"))
	raw = append(raw, sizedTARRecord(t, "main.typ", "ok", 1)...)
	path := gzipTAR(t, append(raw, make([]byte, 1024)...))
	assertArchiveFailure(t, ArchiveSource{Path: path, Options: ArchiveOptions{Limits: Limits{MaxFileBytes: 1}}}, ErrLimit)
	raw = tarRecord(t, "GlobalHead.0", tar.TypeXGlobalHeader, paxRecord("path", "same.typ"))
	raw = append(raw, tarRecord(t, "first.typ", tar.TypeReg, []byte("ok"))...)
	raw = append(raw, tarRecord(t, "second.typ", tar.TypeReg, []byte("ok"))...)
	path = gzipTAR(t, append(raw, make([]byte, 1024)...))
	assertArchiveFailure(t, ArchiveSource{Path: path}, ErrUnsafePath)
}
