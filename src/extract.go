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
	"math"
	"os"
	"strings"

	"github.com/c2h5oh/datasize"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/klauspost/compress/zstd"
)

var ErrArchiveTooLarge = errors.New("archive too large")

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

const BlobReferencePrefix = "/git/blobs/"

type UnresolvedRefError struct {
	missing []string
}

func (err UnresolvedRefError) Error() string {
	return fmt.Sprintf("%d unresolved blob references", len(err.missing))
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
	manifest *Manifest, fileName string, target string,
	index map[string]*Entry, missing *[]string,
) *Entry {
	if hash, found := strings.CutPrefix(target, BlobReferencePrefix); found {
		if entry, found := index[hash]; found {
			manifest.Contents[fileName] = entry
			return entry
		} else {
			*missing = append(*missing, hash)
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
	missing := []string{}
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
			entry := addSymlinkOrBlobReference(
				manifest, fileName, header.Linkname, index, &missing)
			dataBytesRecycled += entry.GetOriginalSize()
		case tar.TypeDir:
			AddDirectory(manifest, fileName)
		default:
			AddProblem(manifest, fileName, "tar: unsupported type '%c'", header.Typeflag)
			continue
		}
	}

	if len(missing) > 0 {
		return nil, UnresolvedRefError{missing}
	}

	// Ensure parent directories exist for all entries.
	EnsureLeadingDirectories(manifest)

	logc.Printf(ctx,
		"reuse: %s recycled, %s transferred\n",
		datasize.ByteSize(dataBytesRecycled).HR(),
		datasize.ByteSize(dataBytesTransferred).HR(),
	)

	return manifest, nil
}

// Used for zstd decompression inside zip files, it is recommended to share this.
var zstdDecomp = zstd.ZipDecompressor()

func ExtractZip(ctx context.Context, reader io.Reader, oldManifest *Manifest) (*Manifest, error) {
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}

	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}

	// Support zstd compression inside zip files.
	archive.RegisterDecompressor(zstd.ZipMethodWinZip, zstdDecomp)
	archive.RegisterDecompressor(zstd.ZipMethodPKWare, zstdDecomp)

	// Detect and defuse zipbombs.
	var totalSize uint64
	for _, file := range archive.File {
		if totalSize+file.UncompressedSize64 < totalSize {
			// Would overflow
			totalSize = math.MaxUint64
			break
		}
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
	missing := []string{}
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
				entry := addSymlinkOrBlobReference(
					manifest, file.Name, string(fileData), index, &missing)
				dataBytesRecycled += entry.GetOriginalSize()
			} else {
				AddFile(manifest, file.Name, fileData)
				dataBytesTransferred += int64(len(fileData))
			}
		}
	}

	if len(missing) > 0 {
		return nil, UnresolvedRefError{missing}
	}

	// Ensure parent directories exist for all entries.
	EnsureLeadingDirectories(manifest)

	logc.Printf(ctx,
		"reuse: %s recycled, %s transferred\n",
		datasize.ByteSize(dataBytesRecycled).HR(),
		datasize.ByteSize(dataBytesTransferred).HR(),
	)

	return manifest, nil
}

