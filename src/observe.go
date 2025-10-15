package main

import (
	"context"
	"io"
	"log"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"runtime/debug"
	"strconv"
	"time"

	slogmulti "github.com/samber/slog-multi"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/getsentry/sentry-go"
	sentryhttp "github.com/getsentry/sentry-go/http"
	sentryslog "github.com/getsentry/sentry-go/slog"
)

func hasSentry() bool {
	return os.Getenv("SENTRY_DSN") != ""
}

func InitObservability() {
	debug.SetPanicOnFault(true)

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

	if hasSentry() {
		enableLogs := false
		if value, err := strconv.ParseBool(os.Getenv("SENTRY_LOGS")); err == nil {
			enableLogs = value
		}

		enableTracing := false
		if value, err := strconv.ParseBool(os.Getenv("SENTRY_TRACING")); err == nil {
			enableTracing = value
		}

		options := sentry.ClientOptions{}
		options.Environment = environment
		options.EnableLogs = enableLogs
		options.EnableTracing = enableTracing
		options.TracesSampleRate = 1
		switch environment {
		case "development", "staging":
		default:
			options.BeforeSendTransaction = func(event *sentry.Event, hint *sentry.EventHint) *sentry.Event {
				sampleRate := 0.05
				if trace, ok := event.Contexts["trace"]; ok {
					if data, ok := trace["data"].(map[string]any); ok {
						if method, ok := data["http.request.method"].(string); ok {
							switch method {
							case "PUT", "DELETE", "POST":
								sampleRate = 1
							default:
								duration := event.Timestamp.Sub(event.StartTime)
								threshold := time.Duration(config.Observability.SlowResponseThreshold)
								if duration >= threshold {
									sampleRate = 1
								}
							}
						}
					}
				}
				if rand.Float64() < sampleRate {
					return event
				}
				return nil
			}
		}
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
	if hasSentry() {
		handler = func(next http.Handler) http.Handler {
			next = sentryhttp.New(sentryhttp.Options{
				Repanic: true,
			}).Handle(handler)

			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Prevent the Sentry SDK from continuing traces as we don't use this feature.
				r.Header.Del(sentry.SentryTraceHeader)
				r.Header.Del(sentry.SentryBaggageHeader)

				next.ServeHTTP(w, r)
			})
		}(handler)
	}

	return handler
}

type noopSpan struct{}

func (span noopSpan) Finish() {}

func ObserveFunction(
	ctx context.Context, funcName string, data ...any,
) (
	interface{ Finish() }, context.Context,
) {
	switch {
	case hasSentry():
		span := sentry.StartSpan(ctx, "function")
		span.Description = funcName
		ObserveData(span.Context(), data...)
		return span, span.Context()
	default:
		return noopSpan{}, ctx
	}
}

func ObserveData(ctx context.Context, data ...any) {
	if span := sentry.SpanFromContext(ctx); span != nil {
		for i := 0; i < len(data); i += 2 {
			name, value := data[i], data[i+1]
			span.SetData(name.(string), value)
		}
	}
}

var (
	blobsRetrievedCount = promauto.NewCounter(prometheus.CounterOpts{
		Name: "git_pages_blobs_retrieved",
		Help: "Count of blobs retrieved",
	})
	blobsRetrievedBytes = promauto.NewCounter(prometheus.CounterOpts{
		Name: "git_pages_blobs_retrieved_bytes",
		Help: "Total size in bytes of blobs retrieved",
	})

	blobsStoredCount = promauto.NewCounter(prometheus.CounterOpts{
		Name: "git_pages_blobs_stored",
		Help: "Count of blobs stored",
	})
	blobsStoredBytes = promauto.NewCounter(prometheus.CounterOpts{
		Name: "git_pages_blobs_stored_bytes",
		Help: "Total size in bytes of blobs stored",
	})

	manifestsRetrievedCount = promauto.NewCounter(prometheus.CounterOpts{
		Name: "git_pages_manifests_retrieved",
		Help: "Count of manifests retrieved",
	})
)

type observedBackend struct {
	inner Backend
}

var _ Backend = (*observedBackend)(nil)

func NewObservedBackend(backend Backend) Backend {
	return &observedBackend{inner: backend}
}

func (backend *observedBackend) GetBlob(
	ctx context.Context,
	name string,
) (
	reader io.ReadSeeker,
	size uint64,
	mtime time.Time,
	err error,
) {
	span, ctx := ObserveFunction(ctx, "GetBlob", "blob.name", name)
	if reader, size, mtime, err = backend.inner.GetBlob(ctx, name); err == nil {
		ObserveData(ctx, "blob.size", size)
		blobsRetrievedCount.Inc()
		blobsRetrievedBytes.Add(float64(size))
	}
	span.Finish()
	return
}

func (backend *observedBackend) PutBlob(ctx context.Context, name string, data []byte) (err error) {
	span, ctx := ObserveFunction(ctx, "PutBlob", "blob.name", name, "blob.size", len(data))
	if err = backend.inner.PutBlob(ctx, name, data); err == nil {
		blobsStoredCount.Inc()
		blobsStoredBytes.Add(float64(len(data)))
	}
	span.Finish()
	return
}

func (backend *observedBackend) DeleteBlob(ctx context.Context, name string) (err error) {
	span, ctx := ObserveFunction(ctx, "DeleteBlob", "blob.name", name)
	err = backend.inner.DeleteBlob(ctx, name)
	span.Finish()
	return
}

func (backend *observedBackend) GetManifest(
	ctx context.Context,
	name string,
	opts GetManifestOptions,
) (
	manifest *Manifest,
	err error,
) {
	span, ctx := ObserveFunction(ctx, "GetManifest", "manifest.name", name)
	if manifest, err = backend.inner.GetManifest(ctx, name, opts); err == nil {
		manifestsRetrievedCount.Inc()
	}
	span.Finish()
	return
}

func (backend *observedBackend) StageManifest(ctx context.Context, manifest *Manifest) (err error) {
	span, ctx := ObserveFunction(ctx, "StageManifest")
	err = backend.inner.StageManifest(ctx, manifest)
	span.Finish()
	return
}

func (backend *observedBackend) CommitManifest(ctx context.Context, name string, manifest *Manifest) (err error) {
	span, ctx := ObserveFunction(ctx, "CommitManifest", "manifest.name", name)
	err = backend.inner.CommitManifest(ctx, name, manifest)
	span.Finish()
	return
}

func (backend *observedBackend) DeleteManifest(ctx context.Context, name string) (err error) {
	span, ctx := ObserveFunction(ctx, "DeleteManifest", "manifest.name", name)
	err = backend.inner.DeleteManifest(ctx, name)
	span.Finish()
	return
}

func (backend *observedBackend) CheckDomain(ctx context.Context, domain string) (found bool, err error) {
	span, ctx := ObserveFunction(ctx, "CheckDomain", "manifest.domain", domain)
	found, err = backend.inner.CheckDomain(ctx, domain)
	span.Finish()
	return
}
