package integrations

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

func extractTARGZIP(ctx context.Context, compressed *os.File, workspace *ownedWorkspace, root *os.Root, limits Limits) error {
	if err := expandGZIP(ctx, compressed, workspace, limits.MaxTotalBytes); err != nil {
		return err
	}
	return extractTAR(ctx, workspace, root, limits)
}

// expandGZIP verifies the entire gzip member, including its footer. A separate
// owned tar capture bounds metadata, padding and all bytes after the tar terminator.
// Additional gzip members and compressed trailing data are rejected explicitly.
func expandGZIP(ctx context.Context, compressed *os.File, workspace *ownedWorkspace, limit int64) (err error) {
	buffered := bufio.NewReader(compressed)
	gz, err := gzip.NewReader(buffered)
	if err != nil {
		return sourceErr(ErrorArchive, "open gzip", "", err)
	}
	defer func() { err = errors.Join(err, gz.Close()) }()
	gz.Multistream(false)
	expanded, err := workspace.root.OpenFile("expanded.tar", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return sourceErr(ErrorInput, "capture tar", "", err)
	}
	defer func() { err = errors.Join(err, expanded.Close()) }()
	if _, err := copyBounded(ctx, expanded, gz, limit); err != nil {
		return sourceErr(ErrorArchive, "expand gzip", "", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := buffered.ReadByte(); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("additional gzip member or compressed trailing data")
		}
		return sourceErr(ErrorArchive, "gzip tail", "", err)
	}
	return nil
}

func extractTAR(ctx context.Context, workspace *ownedWorkspace, root *os.Root, limits Limits) (err error) {
	file, err := workspace.root.Open("expanded.tar")
	if err != nil {
		return sourceErr(ErrorInput, "open tar capture", "", err)
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return sourceErr(ErrorArchive, "stat tar", "", err)
	}
	plans, err := planTAR(ctx, file, info.Size(), limits)
	if err != nil {
		return err
	}
	writer := treeWriter{root: root}
	for _, plan := range plans {
		if err := ctx.Err(); err != nil {
			return err
		}
		reader := io.NewSectionReader(file, plan.dataOffset, plan.entry.size)
		read, err := writer.writeEntry(ctx, plan.entry, reader, limits.MaxFileBytes)
		if err != nil {
			return sourceErr(ErrorArchive, "extract tar", plan.entry.name, err)
		}
		if read != plan.entry.size {
			return sourceErr(ErrorArchive, "extract tar", plan.entry.name, io.ErrUnexpectedEOF)
		}
	}
	return nil
}

type tarEntryPlan struct {
	entry      preparedEntry
	dataOffset int64
}

// planTAR validates all physical records before any tree writes. archive/tar
// decodes base headers, while this planner applies persistent global PAX values
// before local overrides and GNU long names. Effective sizes determine payload
// boundaries; bytes named by global metadata cannot be discarded as padding.
func planTAR(ctx context.Context, file *os.File, total int64, limits Limits) ([]tarEntryPlan, error) {
	var offset int64
	var count int64
	var plans []tarEntryPlan
	registry := make(pathRegistry)
	globals := make(map[string]string)
	var local map[string]string
	var longName string
	pendingMetadata := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var block [512]byte
		if _, err := file.ReadAt(block[:], offset); err != nil {
			return nil, sourceErr(ErrorArchive, "read tar block", "", err)
		}
		if bytes.Equal(block[:], make([]byte, 512)) {
			if pendingMetadata {
				return nil, sourceErr(ErrorArchive, "tar metadata", "", errors.New("metadata has no following file or directory"))
			}
			var second [512]byte
			if _, err := file.ReadAt(second[:], offset+512); err != nil {
				return nil, sourceErr(ErrorArchive, "read tar terminator", "", err)
			}
			if !bytes.Equal(second[:], make([]byte, 512)) {
				return nil, sourceErr(ErrorArchive, "tar terminator", "", errors.New("expected two zero blocks"))
			}
			if err := validateTARTail(ctx, io.NewSectionReader(file, offset+1024, total-offset-1024)); err != nil {
				return nil, err
			}
			return plans, nil
		}
		header, err := physicalTARHeader(block)
		if err != nil {
			return nil, sourceErr(ErrorArchive, "parse tar block", "", err)
		}
		flag := block[156]
		rawDirectory := flag == tar.TypeDir || (flag == tar.TypeRegA && strings.HasSuffix(header.Name, "/"))
		if _, err := portablePath(header.Name, rawDirectory, true); err != nil {
			return nil, sourceErr(ErrorUnsafePath, "validate tar record", header.Name, err)
		}
		metadata := false
		switch flag {
		case tar.TypeReg, tar.TypeRegA, tar.TypeDir:
		case tar.TypeXHeader, tar.TypeXGlobalHeader, tar.TypeGNULongName:
			metadata = true
			if flag != tar.TypeXGlobalHeader {
				pendingMetadata = true
			}
		default:
			return nil, sourceErr(ErrorUnsupportedEntry, "validate tar record", header.Name, errors.New("link, sparse, or special record"))
		}
		size := header.Size
		name := header.Name
		var entry preparedEntry
		if !metadata {
			if value, ok := effectivePAX("path", globals, local); ok {
				name = value
			}
			if longName != "" {
				name = longName
			}
			if value, ok := effectivePAX("size", globals, local); ok {
				size, err = strconv.ParseInt(value, 10, 64)
				if err != nil {
					return nil, sourceErr(ErrorArchive, "PAX effective size", value, err)
				}
			}
			directory := flag == tar.TypeDir || (flag == tar.TypeRegA && strings.HasSuffix(name, "/"))
			entry, err = prepareEntry(registry, name, directory, size, &count, limits)
			if err != nil {
				return nil, err
			}
			local, longName, pendingMetadata = nil, "", false
		}
		if size < 0 || size > total-offset-512 {
			return nil, sourceErr(ErrorArchive, "tar payload", name, io.ErrUnexpectedEOF)
		}
		if metadata {
			// archive/tar applies this same bound to its special-file payloads.
			if size > 1<<20 {
				return nil, sourceErr(ErrorArchive, "tar metadata", header.Name, errors.New("metadata exceeds 1 MiB"))
			}
			var captured bytes.Buffer
			_, err := copyBounded(ctx, &captured, io.NewSectionReader(file, offset+512, size), size)
			if err != nil {
				return nil, sourceErr(ErrorArchive, "read tar metadata", header.Name, err)
			}
			data := captured.Bytes()
			if flag == tar.TypeGNULongName {
				longName = string(data)
				if nul := strings.IndexByte(longName, 0); nul >= 0 {
					if strings.Trim(longName[nul:], "\x00") != "" {
						return nil, sourceErr(ErrorArchive, "GNU long name", header.Name, errors.New("data after name terminator"))
					}
					longName = longName[:nul]
				}
				if _, err := portablePath(longName, strings.HasSuffix(longName, "/"), true); err != nil {
					return nil, sourceErr(ErrorUnsafePath, "GNU long name", longName, err)
				}
			} else {
				parsed, err := parsePAX(ctx, data)
				if err != nil {
					return nil, err
				}
				if flag == tar.TypeXHeader {
					local = parsed
				} else {
					for key, value := range parsed {
						if err := ctx.Err(); err != nil {
							return nil, err
						}
						if value == "" {
							delete(globals, key)
						} else {
							globals[key] = value
						}
					}
				}
			}
		} else {
			read, err := copyBounded(ctx, io.Discard, io.NewSectionReader(file, offset+512, size), limits.MaxFileBytes)
			if err != nil {
				return nil, sourceErr(ErrorArchive, "validate tar data", name, err)
			}
			if read != size {
				return nil, sourceErr(ErrorArchive, "validate tar data", name, io.ErrUnexpectedEOF)
			}
			plans = append(plans, tarEntryPlan{entry: entry, dataOffset: offset + 512})
		}
		padding := (512 - size%512) % 512
		if padding > total-offset-512-size {
			return nil, sourceErr(ErrorArchive, "tar padding", name, io.ErrUnexpectedEOF)
		}
		offset += 512 + size + padding
	}
}

// A local empty value suppresses a global value for this entry; an absent local
// key inherits the active global value. Empty global values delete keys.
func effectivePAX(key string, global, local map[string]string) (string, bool) {
	if value, ok := local[key]; ok {
		return value, value != ""
	}
	value, ok := global[key]
	return value, ok
}

func physicalTARHeader(block [512]byte) (*tar.Header, error) {
	stored, err := strconv.ParseInt(strings.Trim(string(block[148:156]), "\x00 "), 8, 64)
	if err != nil {
		return nil, tar.ErrHeader
	}
	var unsigned, signed int64
	for i, b := range block {
		if i >= 148 && i < 156 {
			b = ' '
		}
		unsigned += int64(b)
		signed += int64(int8(b))
	}
	if stored != unsigned && stored != signed {
		return nil, tar.ErrHeader
	}
	// Decode hidden metadata headers without asking tar.Reader to consume their
	// payload. The original checksum was verified before changing the type byte.
	block[156] = tar.TypeReg
	for i := 148; i < 156; i++ {
		block[i] = ' '
	}
	unsigned = 0
	for _, b := range block {
		unsigned += int64(b)
	}
	copy(block[148:156], fmt.Sprintf("%06o\x00 ", unsigned))
	header, err := tar.NewReader(bytes.NewReader(block[:])).Next()
	if errors.Is(err, tar.ErrInsecurePath) && header != nil {
		err = nil
	}
	return header, err
}

func validateTARTail(ctx context.Context, reader io.Reader) error {
	buffer := make([]byte, 32<<10)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := reader.Read(buffer)
		for _, b := range buffer[:n] {
			if b != 0 {
				return sourceErr(ErrorArchive, "tar tail", "", errors.New("non-padding data after tar terminator"))
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return sourceErr(ErrorArchive, "tar tail", "", err)
		}
	}
}

func parsePAX(ctx context.Context, data []byte) (map[string]string, error) {
	parsed := make(map[string]string)
	for len(data) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		space := bytes.IndexByte(data, ' ')
		if space <= 0 {
			return nil, sourceErr(ErrorArchive, "PAX record", "", tar.ErrHeader)
		}
		length, err := strconv.ParseInt(string(data[:space]), 10, 64)
		if err != nil || length <= int64(space+1) || length > int64(len(data)) || data[length-1] != '\n' {
			return nil, sourceErr(ErrorArchive, "PAX record", "", tar.ErrHeader)
		}
		key, value, ok := strings.Cut(string(data[space+1:length-1]), "=")
		if !ok || key == "" || strings.ContainsRune(key, 0) {
			return nil, sourceErr(ErrorArchive, "PAX record", "", tar.ErrHeader)
		}
		if strings.HasPrefix(key, "GNU.sparse.") || strings.HasPrefix(key, "SCHILY.sparse.") || key == "SCHILY.realsize" {
			return nil, sourceErr(ErrorUnsupportedEntry, "PAX record", key, errors.New("sparse entries are unsupported"))
		}
		switch key {
		case "path", "linkpath":
			if value == "" {
				break
			}
			if _, err := portablePath(value, strings.HasSuffix(value, "/"), true); err != nil {
				return nil, sourceErr(ErrorUnsafePath, "PAX path", value, err)
			}
		case "size":
			if value == "" {
				break
			}
			number, err := strconv.ParseInt(value, 10, 64)
			if err != nil || number < 0 {
				return nil, sourceErr(ErrorArchive, "PAX size", value, errors.Join(tar.ErrHeader, err))
			}
		case "uid", "gid":
			if value == "" {
				break
			}
			if _, err := strconv.ParseInt(value, 10, 64); err != nil {
				return nil, sourceErr(ErrorArchive, "PAX owner", value, errors.Join(tar.ErrHeader, err))
			}
		case "atime", "mtime", "ctime":
			if value == "" {
				break
			}
			seconds, fraction, _ := strings.Cut(value, ".")
			if _, err := strconv.ParseInt(seconds, 10, 64); err != nil {
				return nil, sourceErr(ErrorArchive, "PAX time", value, errors.Join(tar.ErrHeader, err))
			}
			for _, digit := range fraction {
				if digit < '0' || digit > '9' {
					return nil, sourceErr(ErrorArchive, "PAX time", value, tar.ErrHeader)
				}
			}
		case "uname", "gname":
			if strings.ContainsRune(value, 0) {
				return nil, sourceErr(ErrorArchive, "PAX owner name", value, tar.ErrHeader)
			}
		}
		parsed[key] = value
		data = data[length:]
	}
	return parsed, nil
}
