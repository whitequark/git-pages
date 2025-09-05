package main

import (
	"net/http"
	"os"
)

func main() {
	dataDir := os.Args[1]

	Fetch(dataDir, "codeberg.page/.index", "https://codeberg.org/Codeberg/pages-server/", "pages")

	mux := http.NewServeMux()
	mux.HandleFunc("/", Serve(dataDir))
	http.ListenAndServe(":3333", mux)
}
