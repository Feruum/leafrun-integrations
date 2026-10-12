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
	if err := validatePhysicalTAR(ctx, file, info.Size(), limits); err != nil {
		return err
	}
	registry := make(pathRegistry)
	var count int64
	var prepared []preparedEntry
	reader := tar.NewReader(io.NewSectionReader(file, 0, info.Size()))
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil && !(errors.Is(err, tar.ErrInsecurePath) && header != nil) {
			return sourceErr(ErrorArchive, "read tar header", "", err)
		}
		if header.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		directory := header.Typeflag == tar.TypeDir
		if !directory && header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return sourceErr(ErrorUnsupportedEntry, "validate tar", header.Name, errors.New("only ordinary files and directories are supported"))
		}
		entry, err := prepareEntry(registry, header.Name, directory, header.Size, &count, limits)
		if err != nil {
			return err
		}
		allowance := limits.MaxFileBytes
		if directory {
			allowance = 0
		}
		read, err := copyBounded(ctx, io.Discard, reader, allowance)
		if err != nil {
			return sourceErr(ErrorArchive, "validate tar data", header.Name, err)
		}
		if read != entry.size {
			return sourceErr(ErrorArchive, "validate tar data", header.Name, io.ErrUnexpectedEOF)
		}
		prepared = append(prepared, entry)
	}
	reader = tar.NewReader(io.NewSectionReader(file, 0, info.Size()))
	index := 0
	writer := treeWriter{root: root}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil && !(errors.Is(err, tar.ErrInsecurePath) && header != nil) {
			return sourceErr(ErrorArchive, "read tar header", "", err)
		}
		if header.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		entry := prepared[index]
		index++
		read, err := writer.writeEntry(ctx, entry, reader, limits.MaxFileBytes)
		if err != nil {
			return sourceErr(ErrorArchive, "extract tar", header.Name, err)
		}
		if read != entry.size {
			return sourceErr(ErrorArchive, "extract tar", header.Name, io.ErrUnexpectedEOF)
		}
	}
}

// validatePhysicalTAR checks records that tar.Reader normally hides, including
// PAX/GNU metadata names and superseded path values. The standard reader remains
// responsible for header/numeric decoding and effective metadata semantics.
func validatePhysicalTAR(ctx context.Context, file *os.File, total int64, limits Limits) error {
	var offset int64
	var paxSize *int64
	pendingMetadata := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var block [512]byte
		if _, err := file.ReadAt(block[:], offset); err != nil {
			return sourceErr(ErrorArchive, "read tar block", "", err)
		}
		if bytes.Equal(block[:], make([]byte, 512)) {
			if pendingMetadata {
				return sourceErr(ErrorArchive, "tar metadata", "", errors.New("metadata has no following file or directory"))
			}
			var second [512]byte
			if _, err := file.ReadAt(second[:], offset+512); err != nil {
				return sourceErr(ErrorArchive, "read tar terminator", "", err)
			}
			if !bytes.Equal(second[:], make([]byte, 512)) {
				return sourceErr(ErrorArchive, "tar terminator", "", errors.New("expected two zero blocks"))
			}
			return validateTARTail(ctx, io.NewSectionReader(file, offset+1024, total-offset-1024))
		}
		header, err := physicalTARHeader(block)
		if err != nil {
			return sourceErr(ErrorArchive, "parse tar block", "", err)
		}
		flag := block[156]
		directory := flag == tar.TypeDir || (flag == tar.TypeRegA && strings.HasSuffix(header.Name, "/"))
		if _, err := portablePath(header.Name, directory, true); err != nil {
			return sourceErr(ErrorUnsafePath, "validate tar record", header.Name, err)
		}
		metadata := false
		switch flag {
		case tar.TypeReg, tar.TypeRegA, tar.TypeDir:
			pendingMetadata = false
		case tar.TypeXHeader, tar.TypeXGlobalHeader, tar.TypeGNULongName:
			metadata = true
			if flag != tar.TypeXGlobalHeader {
				pendingMetadata = true
			}
		default:
			return sourceErr(ErrorUnsupportedEntry, "validate tar record", header.Name, errors.New("link, sparse, or special record"))
		}
		size := header.Size
		if !metadata && paxSize != nil {
			size = *paxSize
			paxSize = nil
		}
		if directory && size != 0 {
			return sourceErr(ErrorUnsupportedEntry, "validate tar record", header.Name, errors.New("directory has a payload"))
		}
		if !metadata && !directory && (size > limits.MaxFileBytes || size > limits.MaxTotalBytes) {
			return sourceErr(ErrorLimit, "size", header.Name, errors.New("declared tar file size exceeded"))
		}
		if size < 0 || size > total-offset-512 {
			return sourceErr(ErrorArchive, "tar payload", header.Name, io.ErrUnexpectedEOF)
		}
		if metadata {
			// archive/tar applies this same bound to its special-file payloads.
			if size > 1<<20 {
				return sourceErr(ErrorArchive, "tar metadata", header.Name, errors.New("metadata exceeds 1 MiB"))
			}
			var captured bytes.Buffer
			_, err := copyBounded(ctx, &captured, io.NewSectionReader(file, offset+512, size), size)
			if err != nil {
				return sourceErr(ErrorArchive, "read tar metadata", header.Name, err)
			}
			data := captured.Bytes()
			if flag == tar.TypeGNULongName {
				name := string(data)
				if nul := strings.IndexByte(name, 0); nul >= 0 {
					if strings.Trim(name[nul:], "\x00") != "" {
						return sourceErr(ErrorArchive, "GNU long name", header.Name, errors.New("data after name terminator"))
					}
					name = name[:nul]
				}
				if _, err := portablePath(name, strings.HasSuffix(name, "/"), true); err != nil {
					return sourceErr(ErrorUnsafePath, "GNU long name", name, err)
				}
			} else {
				pendingSize, err := validatePAX(ctx, data)
				if err != nil {
					return err
				}
				if flag == tar.TypeXHeader {
					paxSize = pendingSize
				}
			}
		}
		padding := (512 - size%512) % 512
		if padding > total-offset-512-size {
			return sourceErr(ErrorArchive, "tar padding", header.Name, io.ErrUnexpectedEOF)
		}
		offset += 512 + size + padding
	}
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

func validatePAX(ctx context.Context, data []byte) (*int64, error) {
	var size *int64
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
		if !ok || key == "" {
			return nil, sourceErr(ErrorArchive, "PAX record", "", tar.ErrHeader)
		}
		if strings.HasPrefix(key, "GNU.sparse.") || strings.HasPrefix(key, "SCHILY.sparse.") || key == "SCHILY.realsize" {
			return nil, sourceErr(ErrorUnsupportedEntry, "PAX record", key, errors.New("sparse entries are unsupported"))
		}
		switch key {
		case "path", "linkpath":
			if _, err := portablePath(value, strings.HasSuffix(value, "/"), true); err != nil {
				return nil, sourceErr(ErrorUnsafePath, "PAX path", value, err)
			}
		case "size":
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil || parsed < 0 {
				return nil, sourceErr(ErrorArchive, "PAX size", value, tar.ErrHeader)
			}
			size = &parsed
		}
		data = data[length:]
	}
	return size, nil
}
