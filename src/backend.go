// Abstract interface for storage backends; filesystem backend.

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type Backend interface {
	// Retrieve a blob. Returns `reader, mtime, err`.
	GetBlob(name string) (io.ReadSeekCloser, time.Time, error)

	// Store a blob. If a blob called `name` already exists, this function returns `nil` without
	// regards to the old or new contents. It is expected that blobs are content-addressed, i.e.
	// the `name` contains a cryptographic hash of `data`, but the backend is ignorant of this.
	PutBlob(name string, data []byte) error

	// Delete a blob. This is an unconditional operation that can break integrity of manifests.
	DeleteBlob(name string) error

	// Retrieve a manifest.
	GetManifest(name string) (*Manifest, error)

	// Stage a manifest. This operation stores a new version of a manifest, locking any blobs
	// referenced from it in place (for garbage collection purposes) but without any other side
	// effects.
	StageManifest(manifest *Manifest) error

	// Commit a manifest. This is an atomic operation; `GetManifest` calls will return either
	// the old version or the new version of the manifest, never anything else.
	CommitManifest(name string, manifest *Manifest) error

	// Delete a manifest.
	DeleteManifest(name string) error
}

type FSBackend struct {
	blobRoot *os.Root
	siteRoot *os.Root
}

func maybeCreateOpenRoot(dir string, name string) (*os.Root, error) {
	dirName := filepath.Join(dir, name)

	if err := os.Mkdir(dirName, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("mkdir: %s", err)
	}

	root, err := os.OpenRoot(dirName)
	if err != nil {
		return nil, fmt.Errorf("open: %s", err)
	}

	return root, nil
}

func createTempInRoot(root *os.Root, name string, data []byte) (string, error) {
	tempFile, err := os.CreateTemp(root.Name(), name)
	if err != nil {
		return "", fmt.Errorf("mktemp: %s", err)
	}
	_, err = tempFile.Write(data)
	tempFile.Close()
	if err != nil {
		return "", fmt.Errorf("write: %s", err)
	}

	tempPath, err := filepath.Rel(root.Name(), tempFile.Name())
	if err != nil {
		return "", fmt.Errorf("relpath: %s", err)
	}

	return tempPath, nil
}

func NewFSBackend(dir string) (*FSBackend, error) {
	blobRoot, err := maybeCreateOpenRoot(dir, "blob")
	if err != nil {
		return nil, fmt.Errorf("blob: %s", err)
	}
	siteRoot, err := maybeCreateOpenRoot(dir, "site")
	if err != nil {
		return nil, fmt.Errorf("site: %s", err)
	}
	return &FSBackend{blobRoot, siteRoot}, nil
}

func (fs *FSBackend) Backend() Backend {
	return fs
}

func splitBlobName(name string) []string {
	algo, hash, found := strings.Cut(name, "-")
	if found {
		return slices.Concat([]string{algo}, splitBlobName(hash))
	} else {
		return []string{name[0:2], name[2:4], name[4:]}
	}
}

func (fs *FSBackend) GetBlob(name string) (io.ReadSeekCloser, time.Time, error) {
	blobPath := filepath.Join(splitBlobName(name)...)
	stat, err := fs.blobRoot.Stat(blobPath)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("stat: %s", err)
	}
	file, err := fs.blobRoot.Open(blobPath)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("open: %s", err)
	}
	return file, stat.ModTime(), nil
}

func (fs *FSBackend) PutBlob(name string, data []byte) error {
	blobPath := filepath.Join(splitBlobName(name)...)
	blobDir := filepath.Dir(blobPath)

	tempPath, err := createTempInRoot(fs.blobRoot, name, data)
	if err != nil {
		return err
	}

	if err := fs.blobRoot.Chmod(tempPath, 0o444); err != nil {
		return fmt.Errorf("chmod: %s", err)
	}

	if err := fs.blobRoot.MkdirAll(blobDir, 0o755); err != nil {
		return fmt.Errorf("mkdir: %s", err)
	}

	if err := fs.blobRoot.Rename(tempPath, blobPath); err != nil {
		return fmt.Errorf("rename: %s", err)
	}

	return nil
}

func (fs *FSBackend) DeleteBlob(name string) error {
	blobPath := filepath.Join(splitBlobName(name)...)
	return fs.blobRoot.Remove(blobPath)
}

func (fs *FSBackend) GetManifest(name string) (*Manifest, error) {
	data, err := fs.siteRoot.ReadFile(name)
	if err != nil {
		return nil, err
	}

	return DecodeManifest(data)
}

func stagedManifestName(manifestData []byte) string {
	return fmt.Sprintf(".%x", sha256.Sum256(manifestData))
}

func (fs *FSBackend) StageManifest(manifest *Manifest) error {
	manifestData := EncodeManifest(manifest)

	tempPath, err := createTempInRoot(fs.siteRoot, ".manifest", manifestData)
	if err != nil {
		return err
	}

	if err := fs.siteRoot.Rename(tempPath, stagedManifestName(manifestData)); err != nil {
		return fmt.Errorf("rename: %s", err)
	}

	return nil
}

func (fs *FSBackend) CommitManifest(name string, manifest *Manifest) error {
	manifestData := EncodeManifest(manifest)
	manifestHashName := stagedManifestName(manifestData)

	if _, err := fs.siteRoot.Stat(manifestHashName); err != nil {
		return fmt.Errorf("manifest not staged")
	}

	if err := fs.siteRoot.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		return fmt.Errorf("mkdir: %s", err)
	}

	if err := fs.siteRoot.Rename(manifestHashName, name); err != nil {
		return fmt.Errorf("rename: %s", err)
	}

	return nil
}

func (fs *FSBackend) DeleteManifest(name string) error {
	return fs.siteRoot.Remove(name)
}

type S3Backend struct {
	ctx    context.Context
	client *minio.Client
	bucket string
}

func NewS3Backend(
	endpoint string,
	insecure bool,
	accessKeyID string,
	secretAccessKey string,
	region string,
	bucket string,
) (*S3Backend, error) {
	ctx := context.Background()

	client, err := minio.New(config.Backend.S3.Endpoint, &minio.Options{
		Creds: credentials.NewStaticV4(
			config.Backend.S3.AccessKeyID,
			config.Backend.S3.SecretAccessKey,
			"",
		),
		Secure: !config.Backend.S3.Insecure,
	})
	if err != nil {
		return nil, err
	}

	exists, err := client.BucketExists(ctx, config.Backend.S3.Bucket)
	if err != nil {
		return nil, err
	} else if !exists {
		err = client.MakeBucket(ctx, config.Backend.S3.Bucket,
			minio.MakeBucketOptions{Region: config.Backend.S3.Region})
		if err != nil {
			return nil, err
		}
	}

	return &S3Backend{ctx, client, bucket}, nil
}

func (s3 *S3Backend) Backend() Backend {
	return s3
}

func blobObjectName(name string) string {
	return fmt.Sprintf("blob/%s", name)
}

func (s3 *S3Backend) GetBlob(name string) (io.ReadSeekCloser, time.Time, error) {
	log.Printf("s3: get blob %s\n", name)

	object, err := s3.client.GetObject(s3.ctx, s3.bucket, blobObjectName(name),
		minio.GetObjectOptions{})
	if err != nil {
		return nil, time.Time{}, err
	}

	stat, err := object.Stat()
	if err != nil {
		return nil, time.Time{}, err
	}

	return object, stat.LastModified, nil
}

func (s3 *S3Backend) PutBlob(name string, data []byte) error {
	log.Printf("s3: put blob %s (%d bytes)\n", name, len(data))

	_, err := s3.client.StatObject(s3.ctx, s3.bucket, blobObjectName(name),
		minio.GetObjectOptions{})
	if err != nil {
		errResp := minio.ToErrorResponse(err)
		if errResp.Code == "NoSuchKey" {
			_, err := s3.client.PutObject(s3.ctx, s3.bucket, blobObjectName(name),
				bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{})
			if err != nil {
				return err
			}
		} else {
			return err
		}
	}
	return nil // already exists or was created
}

func (s3 *S3Backend) DeleteBlob(name string) error {
	log.Printf("s3: delete blob %s\n", name)

	return s3.client.RemoveObject(s3.ctx, s3.bucket, blobObjectName(name),
		minio.RemoveObjectOptions{})
}

func manifestObjectName(name string) string {
	return fmt.Sprintf("site/%s", name)
}

func stagedManifestObjectName(manifestData []byte) string {
	return fmt.Sprintf("dirty/%x", sha256.Sum256(manifestData))
}

func (s3 *S3Backend) GetManifest(name string) (*Manifest, error) {
	log.Printf("s3: get manifest %s\n", name)

	object, err := s3.client.GetObject(s3.ctx, s3.bucket, manifestObjectName(name),
		minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}

	data, err := io.ReadAll(object)
	if err != nil {
		return nil, err
	}

	return DecodeManifest(data)
}

func (s3 *S3Backend) StageManifest(manifest *Manifest) error {
	data := EncodeManifest(manifest)
	log.Printf("s3: stage manifest %x\n", sha256.Sum256(data))

	_, err := s3.client.PutObject(s3.ctx, s3.bucket, stagedManifestObjectName(data),
		bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{})
	return err
}

func (s3 *S3Backend) CommitManifest(name string, manifest *Manifest) error {
	data := EncodeManifest(manifest)
	log.Printf("s3: commit manifest %x -> %s", sha256.Sum256(data), name)

	// Remove staged object unconditionally (whether commit succeeded or failed), since
	// the upper layer has to retry the complete operation anyway.
	_, putErr := s3.client.PutObject(s3.ctx, s3.bucket, manifestObjectName(name),
		bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{})
	removeErr := s3.client.RemoveObject(s3.ctx, s3.bucket, stagedManifestObjectName(data),
		minio.RemoveObjectOptions{})
	if putErr != nil {
		return putErr
	} else if removeErr != nil {
		return removeErr
	} else {
		return nil
	}
}

func (s3 *S3Backend) DeleteManifest(name string) error {
	log.Printf("s3: delete manifest %s\n", name)

	return s3.client.RemoveObject(s3.ctx, s3.bucket, manifestObjectName(name),
		minio.RemoveObjectOptions{})
}
