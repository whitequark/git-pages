package main

import (
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"

	"google.golang.org/protobuf/proto"
)

func readToManifest(root *os.Root) (*Manifest, error) {
	manifest := Manifest{}
	manifest.Contents = make(map[string]*Entry)
	err := fs.WalkDir(root.FS(), ".", func(path string, dirEntry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		manifestEntry := Entry{}
		if dirEntry.IsDir() {
			manifestEntry.Type = Type_Directory.Enum()
		} else if dirEntry.Type().IsRegular() {
			data, err := root.ReadFile(path)
			if err != nil {
				return err
			}
			manifestEntry.Type = Type_InlineFile.Enum()
			manifestEntry.Size = proto.Uint32(uint32(len(data)))
			manifestEntry.Data = data
		} else if dirEntry.Type().Type() == fs.ModeSymlink {
			target, err := root.Readlink(path)
			if err != nil {
				return err
			}
			manifestEntry.Type = Type_Symlink.Enum()
			manifestEntry.Size = proto.Uint32(uint32(len(target)))
			manifestEntry.Data = []byte(target)
		} else {
			log.Printf("migrate v1: illegal %s/%s\n", root.Name(), path)
		}
		if path == "." {
			path = ""
		}
		manifest.Contents[path] = &manifestEntry
		return nil
	})
	return &manifest, err
}

type ReadDirLinkFS interface { // aaaaahh!!! Why is Go like this!!
	fs.ReadDirFS
	fs.ReadLinkFS
}

func MigrateFromV1(root *os.Root) error {
	data := root.FS().(ReadDirLinkFS)

	domainDirEntries, err := data.ReadDir("www")
	if err != nil {
		return err
	}

	for _, domainDirEntry := range domainDirEntries {
		domain := domainDirEntry.Name()
		if !domainDirEntry.IsDir() {
			return fmt.Errorf("migrate v1: www/%s: not a directory", domain)
		}

		projectDirEntries, err := data.ReadDir(filepath.Join("www", domain))
		if err != nil {
			return err
		}

		for _, projectDirEntry := range projectDirEntries {
			projectName := projectDirEntry.Name()
			if projectDirEntry.Type().Type() != fs.ModeSymlink {
				return fmt.Errorf("migrate v1: www/%s/%s: not a symlink", domain, projectName)
			}

			treeRoot, err := root.OpenRoot(filepath.Join("www", domain, projectName))
			if err != nil {
				return err
			}

			manifest, err := readToManifest(treeRoot)
			if err != nil {
				return fmt.Errorf("migrate v1: read %s/%s: %w", domain, projectName, err)
			}

			_, err = StoreManifest(fmt.Sprintf("%s/%s", domain, projectName), manifest)
			if err != nil {
				return fmt.Errorf("migrate v1: store %s/%s: %w", domain, projectName, err)
			}
		}
	}
	return nil
}
