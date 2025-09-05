package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	securejoin "github.com/cyphar/filepath-securejoin"
	"golang.org/x/sys/unix"
)

const fetchTimeout = 30 * time.Second

func getHost(r *http.Request) string {
	// FIXME: handle IDNA
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		// dirty but the go stdlib doesn't have a "split port if present" function
		host = r.Host
	}
	return host
}

func getPage(dataDir string, w http.ResponseWriter, r *http.Request) error {
	host := getHost(r)

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
			defer file.Close()
			file, err = securejoin.OpenInRoot(dataDir,
				filepath.Join(wwwRoot, requestPath, "index.html"))
		}
	}
	// if whatever we were serving doesn't exist, try to serve `$root/404.html`
	if errors.Is(err, os.ErrNotExist) {
		file, _ = securejoin.OpenInRoot(dataDir, filepath.Join(wwwRoot, "404.html"))
	}

	// acquire read capability to the file being served (if possible)
	reader := io.ReadSeeker(nil)
	if file != nil {
		defer file.Close()
		file, err = securejoin.Reopen(file, unix.O_RDONLY)
		if file != nil {
			defer file.Close()
			reader = file
		}
	}

	// decide on the HTTP status
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			w.WriteHeader(http.StatusNotFound)
			if reader == nil {
				reader = bytes.NewReader([]byte("not found\n"))
			}
		} else {
			w.WriteHeader(http.StatusInternalServerError)
			reader = bytes.NewReader([]byte("internal server error\n"))
		}
		// serve custom 404 page (if any)
		io.Copy(w, reader)
	} else {
		stat, _ := file.Stat()
		http.ServeContent(w, r, path, stat.ModTime(), reader)
	}

	return err
}

type putResult struct {
	head   string
	result FetchResult
	err    error
}

func putPage(dataDir string, w http.ResponseWriter, r *http.Request) error {
	host := getHost(r)

	// path must be either `/` or `/foo/` (`/foo` is accepted as an alias)
	path, _ := strings.CutPrefix(r.URL.Path, "/")
	path, _ = strings.CutSuffix(path, "/")
	if strings.HasPrefix(path, ".") {
		http.Error(w, "this directory name is reserved for system use", http.StatusBadRequest)
		return fmt.Errorf("reserved name")
	} else if strings.Contains(path, "/") {
		http.Error(w, "only one level of nesting is allowed", http.StatusBadRequest)
		return fmt.Errorf("nesting too deep")
	}

	// path `/` corresponds to pseudo-project `.index`
	projectName := ".index"
	if path != "" {
		projectName = path
	}

	requestBody, err := io.ReadAll(r.Body)
	if err != nil {
		return fmt.Errorf("body read: %s", err)
	}

	// request body contains git repository URL `https://codeberg.org/...`
	// request header X-Pages-Branch contains git branch, `pages` by default
	webRoot := fmt.Sprintf("%s/%s", host, projectName)
	repoURL := string(requestBody)
	branch := r.Header.Get("X-Pages-Branch")
	if branch == "" {
		branch = "pages"
	}

	// fetch the updated content with a timeout
	c := make(chan putResult, 1)
	go func() {
		head, result, err := Fetch(dataDir, webRoot, repoURL, branch)
		c <- putResult{head, result, err}
	}()
	select {
	case putResult := <-c:
		if putResult.err == nil {
			w.Header().Add("Content-Location", r.URL.String())
		}
		switch putResult.result {
		case FetchError:
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintln(w, putResult.err)
			return putResult.err
			// HTTP prescribes these response codes to be used
		case FetchNoChange:
			w.WriteHeader(http.StatusNoContent)
		case FetchCreated:
			w.WriteHeader(http.StatusCreated)
		case FetchUpdated:
			w.WriteHeader(http.StatusOK)
		}
		fmt.Fprintln(w, putResult.head)
		return nil
	case <-time.After(fetchTimeout):
		w.WriteHeader(http.StatusGatewayTimeout)
		return fmt.Errorf("fetch timeout")
	}
}

func Serve(dataDir string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Println("serve:", r.Method, r.Host, r.URL)
		err := error(nil)
		switch r.Method {
		case http.MethodGet:
			err = getPage(dataDir, w, r)
		case http.MethodPut:
			err = putPage(dataDir, w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			err = fmt.Errorf("method %s not allowed", r.Method)
		}
		if err != nil {
			log.Println("serve err:", err)
		}
	}
}
