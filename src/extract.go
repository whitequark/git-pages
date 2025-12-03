package git_pages

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/c2h5oh/datasize"
	"github.com/klauspost/compress/zstd"
)

var ErrArchiveTooLarge = errors.New("archive too large")

func boundArchiveStream(reader io.Reader) io.Reader {
	return ReadAtMost(reader, int64(config.Limits.MaxSiteSize.Bytes()),
		fmt.Errorf("%w: %s limit exceeded", ErrArchiveTooLarge, config.Limits.MaxSiteSize.HR()))
}

func ExtractGzip(reader io.Reader, next func(io.Reader) (*Manifest, error)) (*Manifest, error) {
	stream, err := gzip.NewReader(reader)
	if err != nil {
		return nil, err
	}
	defer stream.Close()

	return next(boundArchiveStream(stream))
}

func ExtractZstd(reader io.Reader, next func(io.Reader) (*Manifest, error)) (*Manifest, error) {
	stream, err := zstd.NewReader(reader)
	if err != nil {
		return nil, err
	}
	defer stream.Close()

	return next(boundArchiveStream(stream))
}

func ExtractTar(reader io.Reader) (*Manifest, error) {
	archive := tar.NewReader(reader)

	manifest := NewManifest()
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}

		// For some reason, GNU tar includes any leading `.` path segments in archive filenames,
		// unless there is a `..` path segment anywhere in the input filenames.
		fileName := header.Name
		for {
			if strippedName, found := strings.CutPrefix(fileName, "./"); found {
				fileName = strippedName
			} else {
				break
			}
		}

		switch header.Typeflag {
		case tar.TypeReg:
			fileData, err := io.ReadAll(archive)
			if err != nil {
				return nil, fmt.Errorf("tar: %s: %w", fileName, err)
			}
			AddFile(manifest, fileName, fileData)
		case tar.TypeSymlink:
			AddSymlink(manifest, fileName, header.Linkname)
		case tar.TypeDir:
			AddDirectory(manifest, fileName)
		default:
			AddProblem(manifest, fileName, "tar: unsupported type '%c'", header.Typeflag)
			continue
		}
	}
	return manifest, nil
}

func ExtractZip(reader io.Reader) (*Manifest, error) {
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}

	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}

	// Detect and defuse zipbombs.
	var totalSize uint64
	for _, file := range archive.File {
		totalSize += file.UncompressedSize64
	}
	if totalSize > config.Limits.MaxSiteSize.Bytes() {
		return nil, fmt.Errorf("%w: decompressed size %s exceeds %s limit",
			ErrArchiveTooLarge,
			datasize.ByteSize(totalSize).HR(),
			config.Limits.MaxSiteSize.HR(),
		)
	}

	manifest := NewManifest()
	for _, file := range archive.File {
		if strings.HasSuffix(file.Name, "/") {
			AddDirectory(manifest, file.Name)
		} else {
			fileReader, err := file.Open()
			if err != nil {
				return nil, err
			}
			defer fileReader.Close()

			fileData, err := io.ReadAll(fileReader)
			if err != nil {
				return nil, fmt.Errorf("zip: %s: %w", file.Name, err)
			}

			if file.Mode()&os.ModeSymlink != 0 {
				AddSymlink(manifest, file.Name, string(fileData))
			} else {
				AddFile(manifest, file.Name, fileData)
			}
		}
	}
	return manifest, nil
}
