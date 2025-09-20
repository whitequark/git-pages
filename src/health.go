package main

import (
	"fmt"
	"net/http"
)

func ServeHealth(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/":
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")

	case "/panic":
		panic("explicit panic request")

	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}
