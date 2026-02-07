package git_pages

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
)

var ErrMalformedPatch = errors.New("malformed patch")

type CreateParentsMode int

const (
	RequireParents CreateParentsMode = iota
	CreateParents
)

// Mutates `manifest` according to a tar stream and the following rules:
//   - A character device with major 0 and minor 0 is a "whiteout marker".  When placed
//     at a given path, this path and its entire subtree (if any) are removed from the manifest.
//   - When a directory is placed at a given path, this path and its entire subtree (if any) are
//     removed from the manifest and replaced with the contents of the directory.
func ApplyTarPatch(manifest *Manifest, reader io.Reader, parents CreateParentsMode) error {
	type Node struct {
		entry    *Entry
		children map[string]*Node
	}

	// Extract the manifest contents (which is using a flat hash map) into a directory tree
	// so that recursive delete operations have O(1) complexity. s
	var root *Node
	sortedNames := slices.Sorted(maps.Keys(manifest.GetContents()))
	for _, name := range sortedNames {
		entry := manifest.Contents[name]
		node := &Node{entry: entry}
		if entry.GetType() == Type_Directory {
			node.children = map[string]*Node{}
		}
		if name == "" {
			root = node
		} else {
			segments := strings.Split(name, "/")
			fileName := segments[len(segments)-1]
			iter := root
			for _, segment := range segments[:len(segments)-1] {
				if iter.children == nil {
					panic("malformed manifest (not a directory)")
				} else if _, exists := iter.children[segment]; !exists {
					panic("malformed manifest (node does not exist)")
				} else {
					iter = iter.children[segment]
				}
			}
			iter.children[fileName] = node
		}
	}
	manifest.Contents = map[string]*Entry{}

	// Process the archive as a patch operation.
	archive := tar.NewReader(reader)
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			return err
		}

		segments := strings.Split(normalizeArchiveMemberName(header.Name), "/")
		fileName := segments[len(segments)-1]
		node := root
		for index, segment := range segments[:len(segments)-1] {
			if node.children == nil {
				dirName := strings.Join(segments[:index], "/")
				return fmt.Errorf("%w: %s: not a directory", ErrMalformedPatch, dirName)
			}
			if _, exists := node.children[segment]; !exists {
				switch parents {
				case RequireParents:
					nodeName := strings.Join(segments[:index+1], "/")
					return fmt.Errorf("%w: %s: path not found", ErrMalformedPatch, nodeName)
				case CreateParents:
					node.children[segment] = &Node{
						entry:    NewManifestEntry(Type_Directory, nil),
						children: map[string]*Node{},
					}
				}
			}
			node = node.children[segment]
		}
		if node.children == nil {
			dirName := strings.Join(segments[:len(segments)-1], "/")
			return fmt.Errorf("%w: %s: not a directory", ErrMalformedPatch, dirName)
		}

		switch header.Typeflag {
		case tar.TypeReg:
			fileData, err := io.ReadAll(archive)
			if err != nil {
				return fmt.Errorf("tar: %s: %w", header.Name, err)
			}
			node.children[fileName] = &Node{
				entry: NewManifestEntry(Type_InlineFile, fileData),
			}
		case tar.TypeSymlink:
			node.children[fileName] = &Node{
				entry: NewManifestEntry(Type_Symlink, []byte(header.Linkname)),
			}
		case tar.TypeDir:
			node.children[fileName] = &Node{
				entry:    NewManifestEntry(Type_Directory, nil),
				children: map[string]*Node{},
			}
		case tar.TypeChar:
			if header.Devmajor == 0 && header.Devminor == 0 {
				delete(node.children, fileName)
			} else {
				AddProblem(manifest, header.Name,
					"tar: unsupported chardev %d,%d", header.Devmajor, header.Devminor)
			}
		default:
			AddProblem(manifest, header.Name,
				"tar: unsupported type '%c'", header.Typeflag)
			continue
		}
	}

	// Repopulate manifest contents with the updated directory tree.
	var traverse func([]string, *Node)
	traverse = func(segments []string, node *Node) {
		manifest.Contents[strings.Join(segments, "/")] = node.entry
		for fileName, childNode := range node.children {
			traverse(append(segments, fileName), childNode)
		}
	}
	traverse([]string{}, root)
	return nil
}
