package main

import (
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	securejoin "github.com/cyphar/filepath-securejoin"
	"golang.org/x/sys/unix"
)

func getPage(dataDir string, w http.ResponseWriter, r *http.Request) error {
	// FIXME: handle IDNA
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		// dirty but the go stdlib doesn't have a "split port if present" function
		host = r.Host
	}

	// if the first directory of the path exists under `www/$host`, use it as the root,
	// else use `www/$host/.index`
	path, _ := strings.CutPrefix(r.URL.Path, "/")
	wwwRoot := filepath.Join("www", host, ".index")
	requestPath := path
	if projectName, projectPath, found := strings.Cut(path, "/"); found {
		projectRoot := filepath.Join("www", host, projectName)
		if file, _ := securejoin.OpenInRoot(dataDir, projectRoot); file != nil {
			file.Close()
			wwwRoot, requestPath = projectRoot, projectPath
		}
	}

	// try to serve `$root/$path` first
	file, err := securejoin.OpenInRoot(dataDir, filepath.Join(wwwRoot, requestPath))
	if err == nil {
		// if it's a directory, serve `$root/$path/index.html`
		stat, statErr := file.Stat()
		if statErr == nil && stat.IsDir() {
			file, err = securejoin.OpenInRoot(dataDir,
				filepath.Join(wwwRoot, requestPath, "index.html"))
		}
	}
	// if whatever we were serving doesn't exist, try to serve `$root/404.html`
	if errors.Is(err, os.ErrNotExist) {
		file, _ = securejoin.OpenInRoot(dataDir, filepath.Join(wwwRoot, "404.html"))
	}

	data := []byte(nil)
	if file != nil {
		defer file.Close()
		file, err = securejoin.Reopen(file, unix.O_RDONLY)
		if file != nil {
			defer file.Close()
			data, err = io.ReadAll(file)
		}
	}

	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			w.WriteHeader(http.StatusNotFound)
			if data == nil {
				data = []byte("404 not found\n")
			}
		} else {
			w.WriteHeader(http.StatusInternalServerError)
		}
	} else {
		w.WriteHeader(http.StatusOK)
	}
	if data != nil {
		w.Write(data)
	}

	return err
}

func Serve(dataDir string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Println("serve:", r.Host, r.URL)
		err := getPage(dataDir, w, r)
		if err != nil {
			log.Println("serve err:", err)
		} else {
			log.Println("serve ok")
		}
	}
}
