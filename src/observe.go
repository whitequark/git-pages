package main

import (
	"net/http"
	"os"
	"runtime/debug"

	"github.com/honeybadger-io/honeybadger-go"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func hasHoneybadger() bool {
	return os.Getenv("HONEYBADGER_API_KEY") != ""
}

func InitObservability() {
	if hasHoneybadger() {
		honeybadger.Configure(honeybadger.Configuration{})
		debug.SetPanicOnFault(true)
	}
}

func ObserveHTTPHandler(handler http.Handler) http.Handler {
	if hasHoneybadger() {
		handler = honeybadger.Handler(handler)
	}
	return handler
}

func NewMetricsHTTPHandler() http.Handler {
	return promhttp.Handler()
}
