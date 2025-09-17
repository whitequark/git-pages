package main

import (
	"fmt"
	"log"
	"net/http"
)

func ServeHealth(w http.ResponseWriter, r *http.Request) {
	log.Println("health: ok")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "ok")
}
