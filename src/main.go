package main

import (
	"os"
)

func main() {
	dataDir := os.Args[1]

	Fetch(dataDir, "codeberg.page/index", "https://codeberg.org/Codeberg/pages-server/", "pages")
}
