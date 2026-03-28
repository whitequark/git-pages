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
	"path"
	"strings"

	"github.com/c2h5oh/datasize"
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

func normalizeArchiveMemberName(fileName string) string {
	// Strip the leading slash and any extraneous path segments.
	fileName = path.Clean(fileName)
	fileName = strings.TrimPrefix(fileName, "/")
	if fileName == "." {
		fileName = ""
	}
	return fileName
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

	index := IndexManifestByGitHash(oldManifest)
	missing := []string{}
	manifest := NewManifest()
	hardLinks := map[string]*Entry{}
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}

		fileName := normalizeArchiveMemberName(header.Name)
		if fileName == "" {
			// This must be the root directory. It will be filled in by EnsureLeadingDirectories.
			continue
		}

		switch header.Typeflag {
		case tar.TypeReg:
			fileData, err := io.ReadAll(archive)
			if err != nil {
				return nil, fmt.Errorf("tar: %s: %w", fileName, err)
			}
			entry := AddFile(manifest, fileName, fileData)
			hardLinks[header.Name] = entry
			dataBytesTransferred += int64(len(fileData))
		case tar.TypeSymlink:
			entry := addSymlinkOrBlobReference(
				manifest, fileName, header.Linkname, index, &missing)
			hardLinks[header.Name] = entry
			switch {
			case entry == nil:
				// unresolved blob reference
			case entry.GetType() != Type_Symlink:
				dataBytesRecycled += entry.GetOriginalSize() // resolved blob reference
			default:
				dataBytesTransferred += int64(len(header.Linkname)) // actual symlink
			}
		case tar.TypeLink:
			if entry, found := hardLinks[header.Linkname]; found {
				manifest.Contents[fileName] = entry
			} else {
				AddProblem(manifest, fileName, "tar: invalid hardlink %q", header.Linkname)
			}
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

	index := IndexManifestByGitHash(oldManifest)
	missing := []string{}
	manifest := NewManifest()
	for _, file := range archive.File {
		normalizedName := normalizeArchiveMemberName(file.Name)
		if strings.HasSuffix(file.Name, "/") {
			AddDirectory(manifest, normalizedName)
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
					manifest, normalizedName, string(fileData), index, &missing)
				switch {
				case entry == nil:
					// unresolved blob reference
				case entry.GetType() != Type_Symlink:
					dataBytesRecycled += entry.GetOriginalSize() // resolved blob reference
				default:
					dataBytesTransferred += int64(len(fileData)) // actual symlink
				}
			} else {
				AddFile(manifest, normalizedName, fileData)
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
