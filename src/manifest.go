//go:generate protoc --go_out=. --go_opt=paths=source_relative schema.proto

package git_pages

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/c2h5oh/datasize"
	"github.com/go-git/go-git/v6/plumbing"
	format "github.com/go-git/go-git/v6/plumbing/format/config"
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

func NewManifest() *Manifest {
	return &Manifest{
		Contents: map[string]*Entry{
			"": {Type: Type_Directory.Enum()},
		},
	}
}

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

func EncodeManifest(manifest *Manifest) (data []byte) {
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(manifest)
	if err != nil {
		panic(err)
	}
	return
}

func DecodeManifest(data []byte) (manifest *Manifest, err error) {
	manifest = &Manifest{}
	err = proto.Unmarshal(data, manifest)
	return
}

func NewManifestEntry(type_ Type, data []byte) *Entry {
	entry := &Entry{}
	entry.Type = type_.Enum()
	if data != nil {
		entry.Data = data
		entry.Transform = Transform_Identity.Enum()
		entry.OriginalSize = proto.Int64(int64(len(data)))
		entry.CompressedSize = proto.Int64(int64(len(data)))
	}
	return entry
}

func AddFile(manifest *Manifest, fileName string, data []byte) *Entry {
	// Fill in `git_hash` even for files not originating from git using the SHA256 algorithm;
	// we use this primarily for incremental archive uploads, but when support for git SHA256
	// repositories is complete, archive uploads and git checkouts will have cross-support for
	// incremental updates.
	hasher := plumbing.NewHasher(format.SHA256, plumbing.BlobObject, int64(len(data)))
	hasher.Write(data)
	entry := NewManifestEntry(Type_InlineFile, data)
	entry.GitHash = proto.String(hasher.Sum().String())
	manifest.Contents[fileName] = entry
	return entry
}

func AddSymlink(manifest *Manifest, fileName string, target string) *Entry {
	if path.IsAbs(target) {
		AddProblem(manifest, fileName, "absolute symlink: %s", target)
		return nil
	} else {
		entry := NewManifestEntry(Type_Symlink, []byte(target))
		manifest.Contents[fileName] = entry
		return entry
	}
}

func AddDirectory(manifest *Manifest, dirName string) *Entry {
	dirName = strings.TrimSuffix(dirName, "/")
	entry := NewManifestEntry(Type_Directory, nil)
	manifest.Contents[dirName] = entry
	return entry
}

func AddProblem(manifest *Manifest, pathName, format string, args ...any) error {
	cause := fmt.Sprintf(format, args...)
	manifest.Problems = append(manifest.Problems, &Problem{
		Path:  proto.String(pathName),
		Cause: proto.String(cause),
	})
	return fmt.Errorf("%s: %s", pathName, cause)
}

func GetProblemReport(manifest *Manifest) []string {
	var report []string
	for _, problem := range manifest.Problems {
		report = append(report,
			fmt.Sprintf("%s: %s", problem.GetPath(), problem.GetCause()))
	}
	return report
}

func ManifestJSON(manifest *Manifest) []byte {
	json, err := protojson.MarshalOptions{
		Multiline:         true,
		EmitDefaultValues: true,
	}.Marshal(manifest)
	if err != nil {
		panic(err)
	}
	return json
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
		} else if entry.GetType() == Type_InlineFile && entry.GetTransform() == Transform_Identity {
			contentType := mime.TypeByExtension(filepath.Ext(path))
			if contentType == "" {
				contentType = http.DetectContentType(entry.Data[:min(512, len(entry.Data))])
			}
			entry.ContentType = proto.String(contentType)
		} else if entry.GetContentType() == "" {
			panic(fmt.Errorf("DetectContentType encountered invalid entry: %v, %v",
				entry.GetType(), entry.GetTransform()))
		}
	}
}

// The `klauspost/compress/zstd` package recommends reusing a compressor to avoid repeated
// allocations of internal buffers.
var zstdEncoder, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBetterCompression))

// Compress contents of inline files.
func CompressFiles(ctx context.Context, manifest *Manifest) {
	span, _ := ObserveFunction(ctx, "CompressFiles")
	defer span.Finish()

	var originalSize int64
	var compressedSize int64
	for _, entry := range manifest.Contents {
		if entry.GetType() == Type_InlineFile && entry.GetTransform() == Transform_Identity {
			mediaType := getMediaType(entry.GetContentType())
			if strings.HasPrefix(mediaType, "video/") || strings.HasPrefix(mediaType, "audio/") {
				continue
			}
			compressedData := zstdEncoder.EncodeAll(entry.GetData(),
				make([]byte, 0, entry.GetOriginalSize()))
			if int64(len(compressedData)) < entry.GetOriginalSize() {
				entry.Data = compressedData
				entry.Transform = Transform_Zstd.Enum()
				entry.CompressedSize = proto.Int64(int64(len(entry.Data)))
			}
		}
		originalSize += entry.GetOriginalSize()
		compressedSize += entry.GetCompressedSize()
	}
	manifest.OriginalSize = proto.Int64(originalSize)
	manifest.CompressedSize = proto.Int64(compressedSize)

	if originalSize != 0 {
		spaceSaving := (float64(originalSize) - float64(compressedSize)) / float64(originalSize)
		logc.Printf(ctx, "compress: saved %.2f percent (%s to %s)",
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
	// Parse Netlify-style `_redirects`.
	if err := ProcessRedirectsFile(manifest); err != nil {
		logc.Printf(ctx, "redirects err: %s\n", err)
	} else if len(manifest.Redirects) > 0 {
		logc.Printf(ctx, "redirects ok: %d rules\n", len(manifest.Redirects))
	}

	// Check if any redirects are unreachable.
	LintRedirects(manifest)

	// Parse Netlify-style `_headers`.
	if err := ProcessHeadersFile(manifest); err != nil {
		logc.Printf(ctx, "headers err: %s\n", err)
	} else if len(manifest.Headers) > 0 {
		logc.Printf(ctx, "headers ok: %d rules\n", len(manifest.Headers))
	}

	// Sniff content type like `http.ServeContent`.
	DetectContentType(manifest)

	// Opportunistically compress blobs (must be done last).
	CompressFiles(ctx, manifest)

	return nil
}

var ErrSiteTooLarge = errors.New("site too large")
var ErrManifestTooLarge = errors.New("manifest too large")

// Uploads inline file data over certain size to the storage backend. Returns a copy of
// the manifest updated to refer to an external content-addressable store.
func StoreManifest(
	ctx context.Context, name string, manifest *Manifest, opts ModifyManifestOptions,
) (*Manifest, error) {
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
	for name, entry := range manifest.Contents {
		cannotBeInlined := entry.GetType() == Type_InlineFile &&
			entry.GetCompressedSize() > int64(config.Limits.MaxInlineFileSize.Bytes())
		if cannotBeInlined {
			dataHash := sha256.Sum256(entry.Data)
			extManifest.Contents[name] = &Entry{
				Type:           Type_ExternalFile.Enum(),
				OriginalSize:   entry.OriginalSize,
				CompressedSize: entry.CompressedSize,
				Data:           fmt.Appendf(nil, "sha256-%x", dataHash),
				Transform:      entry.Transform,
				ContentType:    entry.ContentType,
				GitHash:        entry.GitHash,
			}
		} else {
			extManifest.Contents[name] = entry
		}
	}

	// Compute the total and deduplicated storage size.
	totalSize := int64(0)
	blobSizes := map[string]int64{}
	for _, entry := range manifest.Contents {
		totalSize += entry.GetOriginalSize()
		if entry.GetType() == Type_ExternalFile {
			blobSizes[string(entry.Data)] = entry.GetCompressedSize()
		}
	}
	if uint64(totalSize) > config.Limits.MaxSiteSize.Bytes() {
		return nil, fmt.Errorf("%w: contents size %s exceeds %s limit",
			ErrSiteTooLarge,
			datasize.ByteSize(totalSize).HR(),
			config.Limits.MaxSiteSize.HR(),
		)
	}
	for _, blobSize := range blobSizes {
		*extManifest.StoredSize += blobSize
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
		// Upload external entries (those that were decided as ineligible for being stored inline).
		// If the entry in the original manifest is already an external reference, there's no need
		// to externalize it (and no way for us to do so, since the entry only contains the blob name).
		if entry.GetType() == Type_ExternalFile && manifest.Contents[name].GetType() == Type_InlineFile {
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

	if err := backend.CommitManifest(ctx, name, &extManifest, opts); err != nil {
		if errors.Is(err, ErrDomainFrozen) {
			return nil, err
		} else {
			return nil, fmt.Errorf("commit manifest: %w", err)
		}
	}

	return &extManifest, nil
}
