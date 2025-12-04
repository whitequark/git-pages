package git_pages

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"time"
)

type Flusher interface {
	Flush() error
}

// Inverse of `ExtractTar`.
func CollectTar(
	context context.Context, writer io.Writer, manifest *Manifest, metadata ManifestMetadata,
) (
	err error,
) {
	archive := tar.NewWriter(writer)

	appendFile := func(header *tar.Header, data []byte, transform Transform) (err error) {
		switch transform {
		case Transform_Identity:
		case Transform_Zstd:
			data, err = zstdDecoder.DecodeAll(data, []byte{})
			if err != nil {
				return fmt.Errorf("zstd: %s: %w", header.Name, err)
			}
		default:
			return fmt.Errorf("%s: unexpected transform", header.Name)
		}
		header.Size = int64(len(data))

		err = archive.WriteHeader(header)
		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}
		_, err = archive.Write(data)
		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}
		return
	}

	for fileName, entry := range manifest.Contents {
		var header tar.Header
		if fileName == "" {
			continue
		}
		header.Name = fileName

		switch entry.GetType() {
		case Type_Directory:
			header.Typeflag = tar.TypeDir
			header.Mode = 0755
			header.ModTime = metadata.LastModified
			err = appendFile(&header, nil, Transform_Identity)

		case Type_InlineFile:
			header.Typeflag = tar.TypeReg
			header.Mode = 0644
			header.ModTime = metadata.LastModified
			err = appendFile(&header, entry.GetData(), entry.GetTransform())

		case Type_ExternalFile:
			var blobReader io.Reader
			var blobMtime time.Time
			var blobData []byte
			blobReader, _, blobMtime, err = backend.GetBlob(context, string(entry.Data))
			if err != nil {
				return
			}
			blobData, _ = io.ReadAll(blobReader)
			header.Typeflag = tar.TypeReg
			header.Mode = 0644
			header.ModTime = blobMtime
			err = appendFile(&header, blobData, entry.GetTransform())

		case Type_Symlink:
			header.Typeflag = tar.TypeSymlink
			header.Mode = 0644
			header.ModTime = metadata.LastModified
			err = appendFile(&header, entry.GetData(), Transform_Identity)

		default:
			panic(fmt.Errorf("CollectTar encountered invalid entry: %v, %v",
				entry.GetType(), entry.GetTransform()))
		}
		if err != nil {
			return err
		}
	}

	if redirects := CollectRedirectsFile(manifest); redirects != "" {
		err = appendFile(&tar.Header{
			Name:     RedirectsFileName,
			Typeflag: tar.TypeReg,
			Mode:     0644,
			ModTime:  metadata.LastModified,
		}, []byte(redirects), Transform_Identity)
		if err != nil {
			return err
		}
	}

	if headers := CollectHeadersFile(manifest); headers != "" {
		err = appendFile(&tar.Header{
			Name:     HeadersFileName,
			Typeflag: tar.TypeReg,
			Mode:     0644,
			ModTime:  metadata.LastModified,
		}, []byte(headers), Transform_Identity)
		if err != nil {
			return err
		}
	}

	err = archive.Flush()
	if err != nil {
		return fmt.Errorf("tar: %w", err)
	}

	flusher, ok := writer.(Flusher)
	if ok {
		err = flusher.Flush()
	}
	return err
}
