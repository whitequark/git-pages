package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"google.golang.org/protobuf/proto"
)

func ExtractTar(reader io.Reader) (*Manifest, error) {
	archive := tar.NewReader(reader)

	manifest := Manifest{
		Contents: map[string]*Entry{
			"": {Type: Type_Directory.Enum()},
		},
	}
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}

		fileName := strings.TrimSuffix(header.Name, "/")
		manifestEntry := Entry{}
		switch header.Typeflag {
		case tar.TypeReg:
			fileData := make([]byte, header.Size)
			length, err := archive.Read(fileData)
			if !(length == int(header.Size) && err == io.EOF) {
				return nil, fmt.Errorf("tar: read: %w (expected %d bytes, read %d)",
					err, header.Size, length)
			}

			manifestEntry.Type = Type_InlineFile.Enum()
			manifestEntry.Size = proto.Uint32(uint32(header.Size))
			manifestEntry.Data = fileData

		case tar.TypeSymlink:
			manifestEntry.Type = Type_Symlink.Enum()
			manifestEntry.Size = proto.Uint32(uint32(header.Size))
			manifestEntry.Data = []byte(header.Linkname)

		case tar.TypeDir:
			manifestEntry.Type = Type_Directory.Enum()

		default:
			AddProblem(&manifest, fileName, "unsupported type '%c'", header.Typeflag)
			continue
		}
		manifest.Contents[fileName] = &manifestEntry
	}
	return &manifest, nil
}

var errZipBomb = errors.New("zip file size limit exceeded")

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
	if totalSize > SiteSizeMax {
		return nil, fmt.Errorf("%w: %d > %d bytes", errZipBomb, totalSize, SiteSizeMax)
	}

	manifest := Manifest{
		Contents: map[string]*Entry{
			"": {Type: Type_Directory.Enum()},
		},
	}
	for _, file := range archive.File {
		manifestEntry := Entry{}
		if !strings.HasSuffix(file.Name, "/") {
			fileReader, err := file.Open()
			if err != nil {
				return nil, err
			}
			defer fileReader.Close()

			fileData, err := io.ReadAll(fileReader)
			if err != nil {
				return nil, fmt.Errorf("zip: read: %w", err)
			}

			manifestEntry.Type = Type_InlineFile.Enum()
			manifestEntry.Size = proto.Uint32(uint32(file.UncompressedSize64))
			manifestEntry.Data = fileData
		} else {
			manifestEntry.Type = Type_Directory.Enum()
		}
		manifest.Contents[strings.TrimSuffix(file.Name, "/")] = &manifestEntry
	}
	return &manifest, nil
}
