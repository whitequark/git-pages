package git_pages

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"slices"

	"github.com/c2h5oh/datasize"
	"github.com/go-git/go-billy/v6/osfs"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/cache"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"github.com/go-git/go-git/v6/plumbing/transport"
	"github.com/go-git/go-git/v6/storage/filesystem"
	"google.golang.org/protobuf/proto"
)

func FetchRepository(
	ctx context.Context, repoURL string, branch string, oldManifest *Manifest,
) (
	*Manifest, error,
) {
	span, ctx := ObserveFunction(ctx, "FetchRepository",
		"git.repository", repoURL, "git.branch", branch)
	defer span.Finish()

	parsedRepoURL, err := url.Parse(repoURL)
	if err != nil {
		return nil, fmt.Errorf("URL parse: %w", err)
	}

	var repo *git.Repository
	var storer *filesystem.Storage
	for _, filter := range []packp.Filter{packp.FilterBlobNone(), packp.Filter("")} {
		var tempDir string
		tempDir, err = os.MkdirTemp("", "fetchRepo")
		if err != nil {
			return nil, fmt.Errorf("mkdtemp: %w", err)
		}
		defer os.RemoveAll(tempDir)

		storer = filesystem.NewStorageWithOptions(
			osfs.New(tempDir, osfs.WithBoundOS()),
			cache.NewObjectLRUDefault(),
			filesystem.Options{
				ExclusiveAccess:      true,
				LargeObjectThreshold: int64(config.Limits.GitLargeObjectThreshold.Bytes()),
			},
		)
		repo, err = git.CloneContext(ctx, storer, nil, &git.CloneOptions{
			Bare:          true,
			URL:           repoURL,
			ReferenceName: plumbing.ReferenceName(branch),
			SingleBranch:  true,
			Depth:         1,
			Tags:          git.NoTags,
			Filter:        filter,
		})
		if err != nil {
			logc.Printf(ctx, "clone err: %s %s filter=%q\n", repoURL, branch, filter)
			continue
		} else {
			logc.Printf(ctx, "clone ok: %s %s filter=%q\n", repoURL, branch, filter)
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("git clone: %w", err)
	}

	ref, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("git head: %w", err)
	}

	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return nil, fmt.Errorf("git commit: %w", err)
	}

	tree, err := repo.TreeObject(commit.TreeHash)
	if err != nil {
		return nil, fmt.Errorf("git tree: %w", err)
	}

	walker := object.NewTreeWalker(tree, true, make(map[plumbing.Hash]bool))
	defer walker.Close()

	// Create a manifest for the tree object corresponding to `branch`, but do not populate it
	// with data yet; instead, record all the blobs we'll need.
	manifest := &Manifest{
		RepoUrl: proto.String(repoURL),
		Branch:  proto.String(branch),
		Commit:  proto.String(ref.Hash().String()),
		Contents: map[string]*Entry{
			"": {Type: Type_Directory.Enum()},
		},
	}
	blobsNeeded := map[plumbing.Hash]*Entry{}
	for {
		name, entry, err := walker.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("git walker: %w", err)
		} else {
			manifestEntry := &Entry{}
			if existingManifestEntry, found := blobsNeeded[entry.Hash]; found {
				// If the same blob is present twice, we only need to fetch it once (and both
				// instances will alias the same `Entry` structure in the manifest).
				manifestEntry = existingManifestEntry
			} else if entry.Mode.IsFile() {
				blobsNeeded[entry.Hash] = manifestEntry
				if entry.Mode == filemode.Symlink {
					manifestEntry.Type = Type_Symlink.Enum()
				} else {
					manifestEntry.Type = Type_InlineFile.Enum()
				}
				manifestEntry.GitHash = proto.String(entry.Hash.String())
			} else if entry.Mode == filemode.Dir {
				manifestEntry.Type = Type_Directory.Enum()
			} else {
				AddProblem(manifest, name, "unsupported mode %#o", entry.Mode)
				continue
			}
			manifest.Contents[name] = manifestEntry
		}
	}

	// Collect checkout statistics.
	var dataBytesFromOldManifest int64
	var dataBytesFromGitCheckout int64
	var dataBytesFromGitTransport int64

	// First, see if we can extract the blobs from the old manifest. This is the preferred option
	// because it avoids both network transfers and recompression. Note that we do not request
	// blobs from the backend under any circumstances to avoid creating a blob existence oracle.
	for _, oldManifestEntry := range oldManifest.GetContents() {
		if hash, ok := plumbing.FromHex(oldManifestEntry.GetGitHash()); ok {
			if manifestEntry, found := blobsNeeded[hash]; found {
				CopyProtoMessage(manifestEntry, oldManifestEntry)
				dataBytesFromOldManifest += oldManifestEntry.GetOriginalSize()
				delete(blobsNeeded, hash)
			}
		}
	}

	// Second, fill the manifest entries with data from the git checkout we just made.
	// This will only succeed if a `blob:none` filter isn't supported and we got a full
	// clone despite asking for a partial clone.
	for hash, manifestEntry := range blobsNeeded {
		if err := readGitBlob(repo, hash, manifestEntry); err == nil {
			dataBytesFromGitCheckout += manifestEntry.GetOriginalSize()
			delete(blobsNeeded, hash)
		}
	}

	// Third, if we still don't have data for some manifest entries, re-establish a git transport
	// and request the missing blobs (only) from the server.
	if len(blobsNeeded) > 0 {
		client, err := transport.Get(parsedRepoURL.Scheme)
		if err != nil {
			return nil, fmt.Errorf("git transport: %w", err)
		}

		endpoint, err := transport.NewEndpoint(repoURL)
		if err != nil {
			return nil, fmt.Errorf("git endpoint: %w", err)
		}

		session, err := client.NewSession(storer, endpoint, nil)
		if err != nil {
			return nil, fmt.Errorf("git session: %w", err)
		}

		connection, err := session.Handshake(ctx, transport.UploadPackService)
		if err != nil {
			return nil, fmt.Errorf("git connection: %w", err)
		}
		defer connection.Close()

		if err := connection.Fetch(ctx, &transport.FetchRequest{
			Wants: slices.Collect(maps.Keys(blobsNeeded)),
			Depth: 1,
			// Git CLI behaves like this, even if the wants above are references to blobs.
			Filter: "blob:none",
		}); err != nil && !errors.Is(err, transport.ErrNoChange) {
			return nil, fmt.Errorf("git blob fetch request: %w", err)
		}

		// All remaining blobs should now be available.
		for hash, manifestEntry := range blobsNeeded {
			if err := readGitBlob(repo, hash, manifestEntry); err != nil {
				return nil, err
			}
			dataBytesFromGitTransport += manifestEntry.GetOriginalSize()
			delete(blobsNeeded, hash)
		}
	}

	logc.Printf(ctx,
		"fetch: %s from old manifest, %s from git checkout, %s from git transport\n",
		datasize.ByteSize(dataBytesFromOldManifest).HR(),
		datasize.ByteSize(dataBytesFromGitCheckout).HR(),
		datasize.ByteSize(dataBytesFromGitTransport).HR(),
	)

	return manifest, nil
}

func readGitBlob(repo *git.Repository, hash plumbing.Hash, entry *Entry) error {
	blob, err := repo.BlobObject(hash)
	if err != nil {
		return fmt.Errorf("git blob %s: %w", hash, err)
	}

	reader, err := blob.Reader()
	if err != nil {
		return fmt.Errorf("git blob open: %w", err)
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("git blob read: %w", err)
	}

	switch entry.GetType() {
	case Type_InlineFile, Type_Symlink:
		// okay
	default:
		panic(fmt.Errorf("readGitBlob encountered invalid entry: %v, %v",
			entry.GetType(), entry.GetTransform()))
	}

	entry.Data = data
	entry.Transform = Transform_Identity.Enum()
	entry.OriginalSize = proto.Int64(blob.Size)
	entry.CompressedSize = proto.Int64(blob.Size)
	return nil
}
