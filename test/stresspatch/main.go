package main

import (
	"archive/tar"
	"bytes"
	"flag"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

func makeInit() []byte {
	writer := bytes.NewBuffer(nil)
	archive := tar.NewWriter(writer)
	archive.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     "index.html",
	})
	archive.Write([]byte{})
	archive.Flush()
	return writer.Bytes()
}

func initSite() {
	req, err := http.NewRequest(http.MethodPut, "http://localhost:3000",
		bytes.NewReader(makeInit()))
	if err != nil {
		panic(err)
	}

	req.Header.Add("Content-Type", "application/x-tar")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
}

func makePatch(n int) []byte {
	writer := bytes.NewBuffer(nil)
	archive := tar.NewWriter(writer)
	archive.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     fmt.Sprintf("%d.txt", n),
	})
	archive.Write([]byte{})
	archive.Flush()
	return writer.Bytes()
}

func patchRequest(n int) int {
	req, err := http.NewRequest(http.MethodPatch, "http://localhost:3000",
		bytes.NewReader(makePatch(n)))
	if err != nil {
		panic(err)
	}

	req.Header.Add("Atomic", "no")
	req.Header.Add("Content-Type", "application/x-tar")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		panic(err)
	}

	fmt.Printf("%d: %s %q\n", n, resp.Status, string(data))
	return resp.StatusCode
}

func concurrentWriter(wg *sync.WaitGroup, n int) {
	for {
		if patchRequest(n) == 200 {
			break
		}
	}
	wg.Done()
}

var count = flag.Int("count", 10, "request count")

func main() {
	flag.Parse()

	initSite()
	time.Sleep(time.Second)

	wg := &sync.WaitGroup{}
	for n := range *count {
		wg.Add(1)
		go concurrentWriter(wg, n)
	}
	wg.Wait()

	success := 0
	for n := range *count {
		resp, err := http.Get(fmt.Sprintf("http://localhost:3000/%d.txt", n))
		if err != nil {
			panic(err)
		}
		if resp.StatusCode == 200 {
			success++
		}
	}
	fmt.Printf("written: %d of %d\n", success, *count)
}
