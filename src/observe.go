package main

import (
	"context"
	"log"
	"log/slog"
	"net/http"
	"os"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/honeybadger-io/honeybadger-go"

	"github.com/getsentry/sentry-go"
	sentryhttp "github.com/getsentry/sentry-go/http"
	sentryslog "github.com/getsentry/sentry-go/slog"

	slogmulti "github.com/samber/slog-multi"
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

	logHandlers := []slog.Handler{}

	switch config.LogFormat {
	case "none":
		// nothing to do
	case "text":
		logHandlers = append(logHandlers,
			slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{}))
	case "json":
		logHandlers = append(logHandlers,
			slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{}))
	default:
		log.Println("unknown log format", config.LogFormat)
	}

	if hasHoneybadger() {
		honeybadger.Configure(honeybadger.Configuration{
			Env: environment,
		})
		debug.SetPanicOnFault(true)
	}

	if hasSentry() {
		enableLogs := false
		if value, err := strconv.ParseBool(os.Getenv("SENTRY_LOGS")); err == nil {
			enableLogs = value
		}

		options := sentry.ClientOptions{}
		options.Environment = environment
		options.EnableLogs = enableLogs
		if err := sentry.Init(options); err != nil {
			log.Fatalf("sentry: %s\n", err)
		}

		if enableLogs {
			logHandlers = append(logHandlers, sentryslog.Option{
				AddSource: true,
			}.NewSentryHandler(context.Background()))
		}
	}

	slog.SetDefault(slog.New(slogmulti.Fanout(logHandlers...)))
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
