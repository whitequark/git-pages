package git_pages

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/c2h5oh/datasize"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/klauspost/compress/zstd"
)

var ErrArchiveTooLarge = errors.New("archive too large")

const BlobReferencePrefix = "/git/blobs/"

func boundArchiveStream(reader io.Reader) io.Reader {
	return ReadAtMost(reader, int64(config.Limits.MaxSiteSize.Bytes()),
		fmt.Errorf("%w: %s limit exceeded", ErrArchiveTooLarge, config.Limits.MaxSiteSize.HR()))
}

func ExtractGzip(
	ctx context.Context, reader io.Reader,
	next func(context.Context, io.Reader) (*Manifest, error),
) (*Manifest, error) {
	stream, err := gzip.NewReader(reader)
	if err != nil {
		return nil, err
	}
	defer stream.Close()

	return next(ctx, boundArchiveStream(stream))
}

func ExtractZstd(
	ctx context.Context, reader io.Reader,
	next func(context.Context, io.Reader) (*Manifest, error),
) (*Manifest, error) {
	stream, err := zstd.NewReader(reader)
	if err != nil {
		return nil, err
	}
	defer stream.Close()

	return next(ctx, boundArchiveStream(stream))
}

// Returns a map of git hash to entry. If `manifest` is nil, returns an empty map.
func indexManifestByGitHash(manifest *Manifest) map[string]*Entry {
	index := map[string]*Entry{}
	for _, entry := range manifest.GetContents() {
		if hash := entry.GetGitHash(); hash != "" {
			if _, ok := plumbing.FromHex(hash); ok {
				index[hash] = entry
			} else {
				panic(fmt.Errorf("index: malformed hash: %s", hash))
			}
		}
	}
	return index
}

func addSymlinkOrBlobReference(
	manifest *Manifest, fileName string, target string, index map[string]*Entry,
) *Entry {
	if hash, found := strings.CutPrefix(target, BlobReferencePrefix); found {
		if entry, found := index[hash]; found {
			manifest.Contents[fileName] = entry
			return entry
		} else {
			AddProblem(manifest, fileName, "unresolved reference: %s", target)
			return nil
		}
	} else {
		return AddSymlink(manifest, fileName, target)
	}
}

func ExtractTar(ctx context.Context, reader io.Reader, oldManifest *Manifest) (*Manifest, error) {
	archive := tar.NewReader(reader)

	var dataBytesRecycled int64
	var dataBytesTransferred int64

	index := indexManifestByGitHash(oldManifest)
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
			dataBytesTransferred += int64(len(fileData))
		case tar.TypeSymlink:
			entry := addSymlinkOrBlobReference(manifest, fileName, header.Linkname, index)
			dataBytesRecycled += entry.GetOriginalSize()
		case tar.TypeDir:
			AddDirectory(manifest, fileName)
		default:
			AddProblem(manifest, fileName, "tar: unsupported type '%c'", header.Typeflag)
			continue
		}
	}

	logc.Printf(ctx,
		"reuse: %s recycled, %s transferred\n",
		datasize.ByteSize(dataBytesRecycled).HR(),
		datasize.ByteSize(dataBytesTransferred).HR(),
	)

	return manifest, nil
}

func ExtractZip(ctx context.Context, reader io.Reader, oldManifest *Manifest) (*Manifest, error) {
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

	var dataBytesRecycled int64
	var dataBytesTransferred int64

	index := indexManifestByGitHash(oldManifest)
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
				entry := addSymlinkOrBlobReference(manifest, file.Name, string(fileData), index)
				dataBytesRecycled += entry.GetOriginalSize()
			} else {
				AddFile(manifest, file.Name, fileData)
				dataBytesTransferred += int64(len(fileData))
			}
		}
	}

	logc.Printf(ctx,
		"reuse: %s recycled, %s transferred\n",
		datasize.ByteSize(dataBytesRecycled).HR(),
		datasize.ByteSize(dataBytesTransferred).HR(),
	)

	return manifest, nil
}
