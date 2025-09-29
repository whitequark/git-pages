package main

import (
	"log"
	"net/http"
	"os"
	"runtime/debug"
	"time"

	"github.com/honeybadger-io/honeybadger-go"

	"github.com/getsentry/sentry-go"
	sentryhttp "github.com/getsentry/sentry-go/http"
)

func hasHoneybadger() bool {
	return os.Getenv("HONEYBADGER_API_KEY") != ""
}

func hasSentry() bool {
	return os.Getenv("SENTRY_DSN") != ""
}

func InitObservability() {
	environment := "development"
	if value, ok := os.LookupEnv("ENVIRONMENT"); ok {
		environment = value
	}

	if hasHoneybadger() {
		honeybadger.Configure(honeybadger.Configuration{
			Env: environment,
		})
		debug.SetPanicOnFault(true)
	}

	if hasSentry() {
		options := sentry.ClientOptions{}
		options.Environment = environment
		options.Dsn = os.Getenv("SENTRY_DSN")
		if err := sentry.Init(options); err != nil {
			log.Fatalf("sentry: %s\n", err)
		}
	}
}

func FiniObservability() {
	if hasSentry() {
		sentry.Flush(2 * time.Second)
	}
}

func ObserveHTTPHandler(handler http.Handler) http.Handler {
	if hasHoneybadger() {
		handler = honeybadger.Handler(handler)
	}

	if hasSentry() {
		handler = sentryhttp.New(sentryhttp.Options{
			Repanic: true,
		}).Handle(handler)
	}

	return handler
}
