//go:generate protoc --go_out=. --go_opt=paths=source_relative schema.proto

package git_pages

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"mime"
	"net/http"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/c2h5oh/datasize"
	"github.com/klauspost/compress/zstd"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

var (
	siteCompressionSpaceSaving = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "git_pages_site_compression_space_saving",
		Help:    "Reduction in site size after compression relative to the uncompressed size",
		Buckets: []float64{.01, .025, .05, .1, .25, .5, .75, 1, 1.25, 1.5, 1.75, 2, 2.5, 5, 10},

		NativeHistogramBucketFactor:     1.1,
		NativeHistogramMaxBucketNumber:  100,
		NativeHistogramMinResetDuration: 10 * time.Minute,
	})
)

func IsManifestEmpty(manifest *Manifest) bool {
	if len(manifest.Contents) > 1 {
		return false
	}
	for name, entry := range manifest.Contents {
		if name == "" && entry.GetType() == Type_Directory {
			return true
		}
	}
	panic(fmt.Errorf("malformed manifest %v", manifest))
}

// Returns `true` if `left` and `right` contain the same files with the same types and data.
func CompareManifest(left *Manifest, right *Manifest) bool {
	if len(left.Contents) != len(right.Contents) {
		return false
	}
	for name, leftEntry := range left.Contents {
		rightEntry := right.Contents[name]
		if rightEntry == nil {
			return false
		}
		if leftEntry.GetType() != rightEntry.GetType() {
			return false
		}
		if !bytes.Equal(leftEntry.Data, rightEntry.Data) {
			return false
		}
	}
	return true
}

func EncodeManifest(manifest *Manifest) []byte {
	result, err := proto.MarshalOptions{Deterministic: true}.Marshal(manifest)
	if err != nil {
		panic(err)
	}
	return result
}

func DecodeManifest(data []byte) (*Manifest, error) {
	manifest := Manifest{}
	err := proto.Unmarshal(data, &manifest)
	return &manifest, err
}

func AddProblem(manifest *Manifest, path, format string, args ...any) error {
	cause := fmt.Sprintf(format, args...)
	manifest.Problems = append(manifest.Problems, &Problem{
		Path:  proto.String(path),
		Cause: proto.String(cause),
	})
	return fmt.Errorf("%s: %s", path, cause)
}

func GetProblemReport(manifest *Manifest) []string {
	var report []string
	for _, problem := range manifest.Problems {
		report = append(report,
			fmt.Sprintf("%s: %s", problem.GetPath(), problem.GetCause()))
	}
	return report
}

func ManifestDebugJSON(manifest *Manifest) string {
	result, err := protojson.MarshalOptions{
		Multiline:         true,
		EmitDefaultValues: true,
	}.Marshal(manifest)
	if err != nil {
		panic(err)
	}
	return string(result)
}

var ErrSymlinkLoop = errors.New("symbolic link loop")

func ExpandSymlinks(manifest *Manifest, inPath string) (string, error) {
	var levels uint
again:
	for levels = 0; levels < config.Limits.MaxSymlinkDepth; levels += 1 {
		parts := strings.Split(inPath, "/")
		for i := 1; i <= len(parts); i++ {
			linkPath := path.Join(parts[:i]...)
			entry := manifest.Contents[linkPath]
			if entry != nil && entry.GetType() == Type_Symlink {
				inPath = path.Join(
					path.Dir(linkPath),
					string(entry.Data),
					path.Join(parts[i:]...),
				)
				continue again
			}
		}
		break
	}
	if levels < config.Limits.MaxSymlinkDepth {
		return inPath, nil
	} else {
		return "", ErrSymlinkLoop
	}
}

// Sniff content type using the same algorithm as `http.ServeContent`.
func DetectContentType(manifest *Manifest) {
	for path, entry := range manifest.Contents {
		if entry.GetType() == Type_Directory || entry.GetType() == Type_Symlink {
			// no Content-Type
		} else if entry.GetType() == Type_InlineFile && entry.GetTransform() == Transform_None {
			contentType := mime.TypeByExtension(filepath.Ext(path))
			if contentType == "" {
				contentType = http.DetectContentType(entry.Data[:512])
			}
			entry.ContentType = proto.String(contentType)
		} else {
			panic(fmt.Errorf("DetectContentType encountered invalid entry: %v, %v",
				entry.GetType(), entry.GetTransform()))
		}
	}
}

// The `clauspost/compress/zstd` package recommends reusing a compressor to avoid repeated
// allocations of internal buffers.
var zstdEncoder, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBetterCompression))

// Compress contents of inline files.
func CompressFiles(ctx context.Context, manifest *Manifest) {
	span, _ := ObserveFunction(ctx, "CompressFiles")
	defer span.Finish()

	var originalSize, compressedSize int64
	for _, entry := range manifest.Contents {
		if entry.GetType() == Type_InlineFile && entry.GetTransform() == Transform_None {
			mtype := getMediaType(entry.GetContentType())
			if strings.HasPrefix(mtype, "video/") || strings.HasPrefix(mtype, "audio/") {
				continue
			}
			originalSize += entry.GetSize()
			compressedData := zstdEncoder.EncodeAll(entry.GetData(), make([]byte, 0, entry.GetSize()))
			if len(compressedData) < int(*entry.Size) {
				entry.Data = compressedData
				entry.Size = proto.Int64(int64(len(entry.Data)))
				entry.Transform = Transform_Zstandard.Enum()
			}
			compressedSize += entry.GetSize()
		}
	}
	manifest.OriginalSize = proto.Int64(originalSize)
	manifest.CompressedSize = proto.Int64(compressedSize)

	if originalSize != 0 {
		spaceSaving := (float64(originalSize) - float64(compressedSize)) / float64(originalSize)
		log.Printf("compress: saved %.2f percent (%s to %s)",
			spaceSaving*100.0,
			datasize.ByteSize(originalSize).HR(),
			datasize.ByteSize(compressedSize).HR(),
		)
		siteCompressionSpaceSaving.
			Observe(spaceSaving)
	}
}

// Apply post-processing steps to the manifest.
// At the moment, there isn't a good way to report errors except to log them on the terminal.
// (Perhaps in the future they could be exposed at `.git-pages/status.txt`?)
func PrepareManifest(ctx context.Context, manifest *Manifest) error {
	// Parse Netlify-style `_redirects`
	if err := ProcessRedirectsFile(manifest); err != nil {
		log.Printf("redirects err: %s\n", err)
	} else if len(manifest.Redirects) > 0 {
		log.Printf("redirects ok: %d rules\n", len(manifest.Redirects))
	}

	// Parse Netlify-style `_headers`
	if err := ProcessHeadersFile(manifest); err != nil {
		log.Printf("headers err: %s\n", err)
	} else if len(manifest.Headers) > 0 {
		log.Printf("headers ok: %d rules\n", len(manifest.Headers))
	}

	// Sniff content type like `http.ServeContent`
	DetectContentType(manifest)

	// Opportunistically compress blobs (must be done last)
	CompressFiles(ctx, manifest)

	return nil
}

var ErrManifestTooLarge = errors.New("manifest too large")

// Uploads inline file data over certain size to the storage backend. Returns a copy of
// the manifest updated to refer to an external content-addressable store.
func StoreManifest(ctx context.Context, name string, manifest *Manifest) (*Manifest, error) {
	span, ctx := ObserveFunction(ctx, "StoreManifest", "manifest.name", name)
	defer span.Finish()

	// Replace inline files over certain size with references to external data.
	extManifest := Manifest{
		RepoUrl:        manifest.RepoUrl,
		Branch:         manifest.Branch,
		Commit:         manifest.Commit,
		Contents:       make(map[string]*Entry),
		Redirects:      manifest.Redirects,
		Headers:        manifest.Headers,
		Problems:       manifest.Problems,
		OriginalSize:   manifest.OriginalSize,
		CompressedSize: manifest.CompressedSize,
		StoredSize:     proto.Int64(0),
	}
	extObjectSizes := make(map[string]int64)
	for name, entry := range manifest.Contents {
		cannotBeInlined := entry.GetType() == Type_InlineFile &&
			entry.GetSize() > int64(config.Limits.MaxInlineFileSize.Bytes())
		if cannotBeInlined {
			dataHash := sha256.Sum256(entry.Data)
			extManifest.Contents[name] = &Entry{
				Type:        Type_ExternalFile.Enum(),
				Size:        entry.Size,
				Data:        fmt.Appendf(nil, "sha256-%x", dataHash),
				Transform:   entry.Transform,
				ContentType: entry.ContentType,
			}
			extObjectSizes[string(dataHash[:])] = entry.GetSize()
		} else {
			extManifest.Contents[name] = entry
		}
	}
	// `extObjectMap` stores size once per object, deduplicating it
	for _, storedSize := range extObjectSizes {
		*extManifest.StoredSize += storedSize
	}

	// Upload the resulting manifest and the blob it references.
	extManifestData := EncodeManifest(&extManifest)
	if uint64(len(extManifestData)) > config.Limits.MaxManifestSize.Bytes() {
		return nil, fmt.Errorf("%w: manifest size %s exceeds %s limit",
			ErrManifestTooLarge,
			datasize.ByteSize(len(extManifestData)).HR(),
			config.Limits.MaxManifestSize,
		)
	}

	if err := backend.StageManifest(ctx, &extManifest); err != nil {
		return nil, fmt.Errorf("stage manifest: %w", err)
	}

	wg := sync.WaitGroup{}
	ch := make(chan error, len(extManifest.Contents))
	for name, entry := range extManifest.Contents {
		if entry.GetType() == Type_ExternalFile {
			wg.Go(func() {
				err := backend.PutBlob(ctx, string(entry.Data), manifest.Contents[name].Data)
				if err != nil {
					ch <- fmt.Errorf("put blob %s: %w", name, err)
				}
			})
		}
	}
	wg.Wait()
	close(ch)
	for err := range ch {
		return nil, err // currently ignores all but 1st error
	}

	if err := backend.CommitManifest(ctx, name, &extManifest); err != nil {
		return nil, fmt.Errorf("commit manifest: %w", err)
	}

	return &extManifest, nil
}
