package git_pages

import (
	"cmp"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	sys "codeberg.org/git-pages/git-pages/src/sys"
	automemlimit "github.com/KimMachineGun/automemlimit/memlimit"
	"github.com/c2h5oh/datasize"
	"github.com/fatih/color"
	"github.com/kankanreno/go-snowflake"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/protobuf/proto"
)

var config *Config
var wildcards []*WildcardPattern
var fallback http.Handler
var backend Backend
var existenceCache ExistenceCache

func configureFeatures(ctx context.Context) (err error) {
	if len(config.Features) > 0 {
		logc.Println(ctx, "features:", strings.Join(config.Features, ", "))
	}
	for _, feature := range config.Features {
		switch feature {
		// Work-in-progress features:
		case "preview", "expiration", "absolute-headers":
		// Permanently unstable features:
		case "codeberg-pages-compat", "relaxed-idna":
		// Stabilized features:
		case "archive-site", "audit", "compress", "patch", "serve-h2c", "existence-cache":
			logc.Printf(ctx, "feature %s has been stabilized", feature)
		// Removed or renamed features:
		case "h2c", "sentry-telemetry-buffer", "site-existence-cache", "domain-existence-cache":
			logc.Printf(ctx, "feature %s has been removed", feature)
		// Invalid features:
		default:
			return fmt.Errorf("unknown feature %q", feature)
		}
	}
	return
}

func configureMemLimit(ctx context.Context) (err error) {
	// Avoid being OOM killed by not garbage collecting early enough.
	memlimitBefore := datasize.ByteSize(debug.SetMemoryLimit(-1))
	automemlimit.SetGoMemLimitWithOpts(
		automemlimit.WithLogger(slog.New(slog.DiscardHandler)),
		automemlimit.WithProvider(
			automemlimit.ApplyFallback(
				automemlimit.FromCgroup,
				automemlimit.FromSystem,
			),
		),
		automemlimit.WithRatio(float64(config.Limits.MaxHeapSizeRatio)),
	)
	memlimitAfter := datasize.ByteSize(debug.SetMemoryLimit(-1))
	if memlimitBefore == memlimitAfter {
		logc.Println(ctx, "memlimit: now", memlimitBefore.HR())
	} else {
		logc.Println(ctx, "memlimit: was", memlimitBefore.HR(), "now", memlimitAfter.HR())
	}
	return
}

// Can only be safely called during initial configuration.
func configureConcurrency(_ context.Context) (err error) {
	putBlobSemaphore = make(chan struct{}, config.Limits.ConcurrentUploads)
	return
}

func configureWildcards(_ context.Context) (err error) {
	newWildcards, err := TranslateWildcards(config.Wildcard)
	if err != nil {
		return err
	} else {
		wildcards = newWildcards
		return nil
	}
}

func configureFallback(_ context.Context) (err error) {
	if config.Fallback.ProxyTo != nil {
		fallbackURL := &config.Fallback.ProxyTo.URL
		fallback = &httputil.ReverseProxy{
			Rewrite: func(r *httputil.ProxyRequest) {
				r.SetURL(fallbackURL)
				r.Out.Host = r.In.Host
				r.Out.Header["X-Forwarded-For"] = r.In.Header["X-Forwarded-For"]
			},
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: config.Fallback.Insecure,
				},
			},
		}
	}
	return
}

// Thread-unsafe, must be called only during initial configuration.
func configureAudit(_ context.Context) (err error) {
	snowflake.SetStartTime(AuditSnowflakeStartTime)
	snowflake.SetMachineID(config.Audit.NodeID)
	return
}

func listen(ctx context.Context, name string, listen string) net.Listener {
	if listen == "-" {
		return nil
	}

	protocol, address, ok := strings.Cut(listen, "/")
	if !ok {
		logc.Fatalf(ctx, "%s: %s: malformed endpoint", name, listen)
	}

	listener, err := net.Listen(protocol, address)
	if err != nil {
		logc.Fatalf(ctx, "%s: %s\n", name, err)
	}

	return listener
}

func panicHandler(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				if err, ok := err.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(http.ErrAbortHandler)
				}
				logc.Printf(r.Context(), "panic: %s %s %s: %s\n%s",
					r.Method, r.Host, r.URL.Path, err, string(debug.Stack()))
				http.Error(w,
					fmt.Sprintf("internal server error: %s", err),
					http.StatusInternalServerError,
				)
			}
		}()
		handler.ServeHTTP(w, r)
	})
}

func serve(ctx context.Context, listener net.Listener, handler http.Handler) {
	if listener != nil {
		server := http.Server{Handler: handler}
		server.Protocols = new(http.Protocols)
		server.Protocols.SetHTTP1(true)
		server.Protocols.SetUnencryptedHTTP2(true)
		logc.Fatalln(ctx, server.Serve(listener))
	}
}

func webRootArg(arg string) string {
	switch strings.Count(arg, "/") {
	case 0:
		return arg + "/.index"
	case 1:
		return arg
	default:
		logc.Fatalln(context.Background(),
			"webroot argument must be either 'domain.tld' or 'domain.tld/dir")
		return ""
	}
}

func fileOutputArg() (writer io.WriteCloser) {
	var err error
	if flag.NArg() == 0 {
		writer = os.Stdout
	} else {
		writer, err = os.Create(flag.Arg(0))
		if err != nil {
			logc.Fatalln(context.Background(), err)
		}
	}
	return
}

func usage() {
	fmt.Fprintf(os.Stderr, "Usage:\n")
	fmt.Fprintf(os.Stderr, "(server) "+
		"git-pages [-config <file>|-no-config]\n")
	fmt.Fprintf(os.Stderr, "(info)   "+
		"git-pages {-version|-print-config-env-vars|-print-config}\n")
	fmt.Fprintf(os.Stderr, "(debug)  "+
		"git-pages {-list-blobs|-list-manifests}\n")
	fmt.Fprintf(os.Stderr, "(debug)  "+
		"git-pages {-get-blob|-get-manifest|-get-archive} <ref> [file]\n")
	fmt.Fprintf(os.Stderr, "(admin)  "+
		"git-pages {-update-site <ref> <file>|-delete-site <ref>}\n")
	fmt.Fprintf(os.Stderr, "(admin)  "+
		"git-pages {-freeze-domain|-unfreeze-domain} <domain>\n")
	fmt.Fprintf(os.Stderr, "(audit)  "+
		"git-pages {-audit-log|-audit-read <id>|-audit-rollback <id>}\n")
	fmt.Fprintf(os.Stderr, "(audit)  "+
		"git-pages {-audit-expire <days>|-audit-detach <domain>/<project>}\n")
	fmt.Fprintf(os.Stderr, "(audit)  "+
		"git-pages  -audit-server <endpoint> <program> [args...]\n")
	fmt.Fprintf(os.Stderr, "(maint)  "+
		"git-pages  -expire-sites [-dry-run]\n")
	fmt.Fprintf(os.Stderr, "(maint)  "+
		"git-pages {-run-migration <name>|-trace-garbage|-analyze-storage}\n")
	flag.PrintDefaults()
}

func Main(versionInfo string) {
	ctx := context.Background()

	flag.Usage = usage
	configTomlPath := flag.String("config", "",
		"load configuration from `filename` (default: 'config.toml')")
	secretTomlPath := flag.String("secrets", "",
		"load additional configuration values from `filename` (default: '$CREDENTIALS_DIRECTORY/secrets.toml' if it exists)")
	noConfig := flag.Bool("no-config", false,
		"run without configuration file (configure via environment variables)")
	printConfigEnvVars := flag.Bool("print-config-env-vars", false,
		"print every recognized configuration environment variable and exit")
	printConfig := flag.Bool("print-config", false,
		"print configuration as JSON and exit")
	listBlobs := flag.Bool("list-blobs", false,
		"enumerate every blob with its metadata")
	listManifests := flag.Bool("list-manifests", false,
		"enumerate every manifest with its metadata")
	getBlob := flag.String("get-blob", "",
		"write contents of `blob` ('sha256-xxxxxxx...xxx')")
	getManifest := flag.String("get-manifest", "",
		"write manifest for `site` (either 'domain.tld' or 'domain.tld/dir') as ProtoJSON")
	getArchive := flag.String("get-archive", "",
		"write archive for `site` (either 'domain.tld' or 'domain.tld/dir') in tar format")
	updateSite := flag.String("update-site", "",
		"update `site` (either 'domain.tld' or 'domain.tld/dir') from archive or repository URL")
	deleteSite := flag.String("delete-site", "",
		"delete `site` (either 'domain.tld' or 'domain.tld/dir')")
	freezeDomain := flag.String("freeze-domain", "",
		"prevent any site uploads to a given `domain`")
	unfreezeDomain := flag.String("unfreeze-domain", "",
		"allow site uploads to a `domain` again after it has been frozen")
	auditLog := flag.Bool("audit-log", false,
		"display audit log")
	auditRead := flag.String("audit-read", "",
		"extract contents of audit record `id` to files '<id>-*'")
	auditRollback := flag.String("audit-rollback", "",
		"restore site from contents of audit record `id`")
	auditExpire := flag.String("audit-expire", "",
		"expire audit records older than `days` old")
	auditDetach := flag.String("audit-detach", "",
		"detach all blobs of audit records for a single `site` (or the entire domain with 'domain.tld/*')")
	auditServer := flag.String("audit-server", "",
		"listen for notifications on `endpoint` and spawn a process for each audit event")
	expireSites := flag.Bool("expire-sites", false,
		"expire sites according to their manifest")
	runMigration := flag.String("run-migration", "",
		"run a store `migration` (one of: create-domain-markers)")
	analyzeStorage := flag.String("analyze-storage", "",
		"display aggregate storage used per domain")
	traceGarbage := flag.Bool("trace-garbage", false,
		"estimate total size of unreachable blobs")
	dryRun := flag.Bool("dry-run", false,
		"print what would be performed instead of executing it")
	version := flag.Bool("version", false,
		"display version")
	flag.Parse()

	if *version {
		fmt.Printf("git-pages %s\n", versionInfo)
		os.Exit(0)
	}

	var cliOperations int
	for _, selected := range []bool{
		*listBlobs,
		*listManifests,
		*getBlob != "",
		*getManifest != "",
		*getArchive != "",
		*updateSite != "",
		*deleteSite != "",
		*freezeDomain != "",
		*unfreezeDomain != "",
		*auditLog,
		*auditRead != "",
		*auditRollback != "",
		*auditExpire != "",
		*auditDetach != "",
		*auditServer != "",
		*expireSites,
		*runMigration != "",
		*analyzeStorage != "",
		*traceGarbage,
	} {
		if selected {
			cliOperations++
		}
	}
	if cliOperations > 1 {
		logc.Fatalln(ctx, "-list-blobs, -list-manifests, -get-blob, -get-manifest, "+
			"-get-archive, -update-site, -delete-site, -freeze-domain, -unfreeze-domain, "+
			"-audit-log, -audit-read, -audit-rollback, -audit-expire, -audit-detach, "+
			"-audit-server, -expire-sites, -run-migration, -analyze-storage, "+
			"and -trace-garbage are mutually exclusive")
	}
	if *dryRun && !(*expireSites) {
		logc.Fatalln(ctx, "-dry-run is not applicable in this context")
	}

	if *configTomlPath != "" && *noConfig {
		logc.Fatalln(ctx, "-no-config and -config are mutually exclusive")
	}

	if *printConfigEnvVars {
		PrintConfigEnvVars()
		return
	}

	var err error
	if *configTomlPath == "" && !*noConfig {
		*configTomlPath = "config.toml"
	}

	if *secretTomlPath == "" && !*noConfig {
		// check for a second config file at $CREDENTIALS_DIRECTORY/secrets.toml, and use it
		if systemdCredentialsDir := os.Getenv("CREDENTIALS_DIRECTORY"); systemdCredentialsDir != "" {
			secretTomlTestPath := path.Join(systemdCredentialsDir, "secrets.toml")
			_, err := os.Stat(secretTomlTestPath)
			if !errors.Is(err, os.ErrNotExist) {
				*secretTomlPath = secretTomlTestPath
			}
		}
	}

	if config, err = Configure(*configTomlPath, *secretTomlPath); err != nil {
		logc.Fatalln(ctx, "config:", err)
	}

	if *printConfig {
		fmt.Println(config.TOML())
		return
	}

	InitObservability()
	defer FiniObservability()

	if err = errors.Join(
		configureFeatures(ctx),
		configureMemLimit(ctx),
		configureConcurrency(ctx),
		configureWildcards(ctx),
		configureFallback(ctx),
		configureAudit(ctx),
	); err != nil {
		logc.Fatalln(ctx, err)
	}

	// The server has its own logic for creating the backend.
	if cliOperations > 0 {
		if backend, err = CreateBackend(ctx, &config.Storage); err != nil {
			logc.Fatalln(ctx, err)
		}

		if existenceCache, err = CreateExistenceCache(ctx); err != nil {
			logc.Fatalln(ctx, err)
		}
	}

	switch {
	case *listBlobs:
		for metadata, err := range backend.EnumerateBlobs(ctx) {
			if err != nil {
				logc.Fatalln(ctx, err)
			}
			fmt.Fprintf(color.Output, "%s %s %s\n",
				metadata.Name,
				color.HiWhiteString(metadata.LastModified.UTC().Format(time.RFC3339)),
				color.HiGreenString(fmt.Sprint(metadata.Size)),
			)
		}

	case *listManifests:
		for metadata, err := range backend.EnumerateManifests(ctx) {
			if err != nil {
				logc.Fatalln(ctx, err)
			}
			fmt.Fprintf(color.Output, "%s %s %s\n",
				metadata.Name,
				color.HiWhiteString(metadata.LastModified.UTC().Format(time.RFC3339)),
				color.HiGreenString(fmt.Sprint(metadata.Size)),
			)
		}

	case *getBlob != "":
		reader, _, err := backend.GetBlob(ctx, *getBlob)
		if err != nil {
			logc.Fatalln(ctx, err)
		}
		io.Copy(fileOutputArg(), reader)

	case *getManifest != "":
		webRoot := webRootArg(*getManifest)
		manifest, _, err := backend.GetManifest(ctx, webRoot, GetManifestOptions{})
		if err != nil {
			logc.Fatalln(ctx, err)
		}
		fmt.Fprintln(fileOutputArg(), string(ManifestJSON(manifest)))

	case *getArchive != "":
		webRoot := webRootArg(*getArchive)
		manifest, metadata, err :=
			backend.GetManifest(ctx, webRoot, GetManifestOptions{})
		if err != nil {
			logc.Fatalln(ctx, err)
		}
		if err = CollectTar(ctx, fileOutputArg(), manifest, metadata); err != nil {
			logc.Fatalln(ctx, err)
		}

	case *updateSite != "":
		ctx = WithPrincipal(ctx)
		GetPrincipal(ctx).CliAdmin = proto.Bool(true)

		if flag.NArg() != 1 {
			logc.Fatalln(ctx, "update source must be provided as the argument")
		}

		sourceURL, err := url.Parse(flag.Arg(0))
		if err != nil {
			logc.Fatalln(ctx, err)
		}

		var result UpdateResult
		if sourceURL.Scheme == "" {
			file, err := os.Open(sourceURL.Path)
			if err != nil {
				logc.Fatalln(ctx, err)
			}
			defer file.Close()

			var contentType string
			switch {
			case strings.HasSuffix(sourceURL.Path, ".zip"):
				contentType = "application/zip"
			case strings.HasSuffix(sourceURL.Path, ".tar"):
				contentType = "application/x-tar"
			case strings.HasSuffix(sourceURL.Path, ".tar.gz"):
				contentType = "application/x-tar+gzip"
			case strings.HasSuffix(sourceURL.Path, ".tar.zst"):
				contentType = "application/x-tar+zstd"
			default:
				log.Fatalf("cannot determine content type from filename %q\n", sourceURL)
			}

			webRoot := webRootArg(*updateSite)
			result = UpdateFromArchive(ctx, webRoot, "", contentType, file, UpdateOptions{})
		} else {
			branch := "pages"
			if sourceURL.Fragment != "" {
				branch, sourceURL.Fragment = sourceURL.Fragment, ""
			}

			webRoot := webRootArg(*updateSite)
			result = UpdateFromRepository(ctx, webRoot, sourceURL.String(), branch, UpdateOptions{})
		}

		switch result.outcome {
		case UpdateError:
			logc.Printf(ctx, "error: %s\n", result.err)
			os.Exit(2)
		case UpdateTimeout:
			logc.Println(ctx, "timeout")
			os.Exit(1)
		case UpdateCreated:
			logc.Println(ctx, "created")
		case UpdateReplaced:
			logc.Println(ctx, "replaced")
		case UpdateDeleted:
			logc.Println(ctx, "deleted")
		case UpdateNoChange:
			logc.Println(ctx, "no-change")
		}

	case *deleteSite != "":
		ctx = WithPrincipal(ctx)
		GetPrincipal(ctx).CliAdmin = proto.Bool(true)

		webRoot := webRootArg(*deleteSite)
		err := backend.DeleteManifest(ctx, webRoot, ModifyManifestOptions{})
		if err != nil {
			logc.Fatalf(ctx, "error: %s\n", err)
		}

		logc.Println(ctx, "deleted")

	case *freezeDomain != "" || *unfreezeDomain != "":
		ctx = WithPrincipal(ctx)
		GetPrincipal(ctx).CliAdmin = proto.Bool(true)

		var domain string
		var freeze bool
		if *freezeDomain != "" {
			domain = *freezeDomain
			freeze = true
		} else {
			domain = *unfreezeDomain
			freeze = false
		}

		if freeze {
			if err = backend.FreezeDomain(ctx, domain); err != nil {
				logc.Fatalln(ctx, err)
			}
			logc.Println(ctx, "frozen")
		} else {
			if err = backend.UnfreezeDomain(ctx, domain); err != nil {
				logc.Fatalln(ctx, err)
			}
			logc.Println(ctx, "thawed")
		}

	case *auditLog:
		records := []*AuditRecord{}
		ids := backend.SearchAuditLog(ctx, SearchAuditLogOptions{})
		for record, err := range backend.GetAuditLogRecords(ctx, ids) {
			if err != nil {
				logc.Fatalln(ctx, err)
			}
			records = append(records, record)
		}

		slices.SortFunc(records, func(a, b *AuditRecord) int {
			return cmp.Compare(a.GetAuditID(), b.GetAuditID())
		})

		for _, record := range records {
			parts := []string{
				record.GetAuditID().String(),
				color.HiWhiteString("%s", record.GetTimestamp().AsTime().UTC().Format(time.RFC3339)),
				fmt.Sprint(record.GetEvent()),
			}
			if record.Manifest != nil && record.Manifest.ExpiresAt != nil {
				parts = append(parts,
					color.HiYellowString("%s", record.DescribeResource()),
				)
			} else {
				parts = append(parts,
					color.HiMagentaString("%s", record.DescribeResource()),
				)
			}
			parts = append(parts,
				color.HiGreenString("%s", record.DescribePrincipal()),
			)
			if record.IsDetached() {
				parts = append(parts,
					color.HiYellowString("(detached)"),
				)
			}
			fmt.Fprintln(color.Output, strings.Join(parts, " "))
		}

	case *auditRead != "":
		id, err := ParseAuditID(*auditRead)
		if err != nil {
			logc.Fatalln(ctx, err)
		}

		record, err := backend.QueryAuditLog(ctx, id)
		if err != nil {
			logc.Fatalln(ctx, err)
		}

		if err = ExtractAuditRecord(ctx, id, record, "."); err != nil {
			logc.Fatalln(ctx, err)
		}

	case *auditRollback != "":
		ctx = WithPrincipal(ctx)
		GetPrincipal(ctx).CliAdmin = proto.Bool(true)

		id, err := ParseAuditID(*auditRollback)
		if err != nil {
			logc.Fatalln(ctx, err)
		}

		record, err := backend.QueryAuditLog(ctx, id)
		if err != nil {
			logc.Fatalln(ctx, err)
		}

		if record.GetManifest() == nil || record.GetDomain() == "" || record.GetProject() == "" {
			logc.Fatalln(ctx, "no manifest in audit record")
		}

		webRoot := path.Join(record.GetDomain(), record.GetProject())
		err = backend.StageManifest(ctx, record.GetManifest())
		if err != nil {
			logc.Fatalln(ctx, err)
		}
		err = backend.CommitManifest(ctx, webRoot, record.GetManifest(), ModifyManifestOptions{})
		if err != nil {
			logc.Fatalln(ctx, err)
		}

	case *auditDetach != "":
		domain, project, found := strings.Cut(*auditDetach, "/")
		if !found || domain == "" || project == "" {
			logc.Fatalln(ctx, "argument to -audit-detach must be in the form of "+
				"'domain.tld/project' or 'domain.tld/*'")
		}

		if project != "*" && project != ".index" {
			if err := ValidateProjectName(project); err != nil {
				logc.Fatalf(ctx, "audit detach: project name: %v\n", err)
			}
		}

		count := 0
		ids := backend.SearchAuditLog(ctx, SearchAuditLogOptions{})
		for record, err := range backend.GetAuditLogRecords(ctx, ids) {
			if err != nil {
				logc.Fatalln(ctx, err)
			}
			if record.GetDomain() == domain && (project == "*" || record.GetProject() == project) {
				if !record.IsDetachable() {
					continue
				} else if !record.IsDetached() {
					logc.Printf(ctx, "detaching audit record %s\n", record.GetAuditID())
					err = backend.DetachAuditRecord(ctx, record.GetAuditID())
					if err != nil {
						logc.Fatalln(ctx, err)
					}
					count++
				} else {
					logc.Printf(ctx, "audit record %s already detached\n", record.GetAuditID())
				}
			}
		}

		if count == 0 {
			logc.Printf(ctx, "no detachable audit records found for %s/%s", domain, project)
		}

	case *auditServer != "":
		if flag.NArg() < 1 {
			logc.Fatalln(ctx, "handler path not provided")
		}

		processor, err := AuditEventProcessor(flag.Arg(0), flag.Args()[1:])
		if err != nil {
			logc.Fatalln(ctx, err)
		}

		serve(ctx, listen(ctx, "audit", *auditServer), ObserveHTTPHandler(processor))

	case *auditExpire != "":
		days, err := strconv.ParseInt(*auditExpire, 10, 0)
		if err != nil {
			logc.Fatalln(ctx, err)
		}

		ids := backend.SearchAuditLog(ctx, SearchAuditLogOptions{
			Until: time.Now().AddDate(0, 0, int(-days)),
		})

		count := 0
		for id, err := range ids {
			if err != nil {
				logc.Fatalln(ctx, err)
			}

			err = backend.ExpireAuditRecord(ctx, id)
			if err != nil {
				logc.Fatalln(ctx, err)
			} else {
				logc.Printf(ctx, "audit: expired record %s\n", id)
				count += 1
			}
		}

		logc.Printf(ctx, "audit: expired %d records\n", count)

	case *expireSites:
		ctx = WithPrincipal(ctx)
		GetPrincipal(ctx).CliAdmin = proto.Bool(true)

		if !config.Feature("expiration") {
			logc.Fatalf(ctx, "expire: feature disabled")
		}

		countExpired, countTransient := 0, 0
		for item, err := range backend.GetAllManifests(ctx) {
			metadata, manifest := item.Splat()
			if err != nil {
				logc.Fatalln(ctx, err)
			}
			if manifest.ExpiresAt != nil {
				countTransient += 1
				if manifest.ExpiresAt.AsTime().Before(time.Now()) {
					if !*dryRun {
						err = backend.ExpireManifest(ctx, metadata.Name)
						if err != nil {
							logc.Fatalln(ctx, err)
						}
					}
					logc.Printf(ctx, "expire: site %s expired at %s",
						metadata.Name, manifest.ExpiresAt.AsTime())
					countExpired += 1
				}
			}
		}

		if *dryRun {
			logc.Printf(ctx, "expire: would expire %d out of %d transient sites (dry run)\n",
				countExpired, countTransient)
		} else {
			logc.Printf(ctx, "expire: expired %d out of %d transient sites\n",
				countExpired, countTransient)
		}

	case *runMigration != "":
		if err = RunMigration(ctx, *runMigration); err != nil {
			logc.Fatalln(ctx, err)
		}

	case *analyzeStorage == "text":
		// datasize.ByteSize.HR() is a little too wide for the 8-char column.
		formatSize := func(b datasize.ByteSize) string {
			switch {
			case b > datasize.GB:
				return fmt.Sprintf("%.1fG", b.GBytes())
			case b > datasize.MB:
				return fmt.Sprintf("%.1fM", b.MBytes())
			case b > datasize.KB:
				return fmt.Sprintf("%.1fK", b.KBytes())
			default:
				return fmt.Sprintf("%dB", b)
			}
		}

		analysis, err := AnalyzeStorage(ctx)
		if err != nil {
			logc.Fatalln(ctx, err)
		}
		slices.SortFunc(analysis, func(a *StorageSize, b *StorageSize) int {
			return cmp.Compare(a.TotalSize, b.TotalSize)
		})

		for _, sizes := range analysis {
			var colorize func(string, ...interface{}) string
			fractionSize :=
				float32(sizes.CurrentBlobSize) / float32(config.Limits.MaxSiteSize.Bytes())
			switch {
			case fractionSize > 0.9:
				colorize = color.HiRedString
			case fractionSize > 0.7:
				colorize = color.HiYellowString
			case fractionSize > 0.1:
				colorize = color.HiGreenString
			default:
				colorize = color.HiWhiteString
			}
			if sizes.Domain != "*" {
				fmt.Fprintf(color.Output, "%s\t%s\t%s\t%s\t%s\n",
					colorize("%.0f%%", fractionSize*100.0),
					colorize("%s", formatSize(datasize.ByteSize(sizes.CurrentSize))),
					formatSize(datasize.ByteSize(sizes.NonCurrentSize)),
					formatSize(datasize.ByteSize(sizes.TotalSize)),
					color.HiMagentaString("%s", sizes.Domain),
				)
			} else {
				fmt.Fprintf(color.Output, "---\t%s\t%s\t%s\t%s\n",
					color.HiCyanString("%s", formatSize(datasize.ByteSize(sizes.CurrentSize))),
					formatSize(datasize.ByteSize(sizes.NonCurrentSize)),
					formatSize(datasize.ByteSize(sizes.TotalSize)),
					color.HiMagentaString("%s", sizes.Domain),
				)
			}
		}
		fmt.Fprintf(color.Output, "Quota%%\tCurrent\tNonCurr\tTotal\tDomain\n")

	case *analyzeStorage == "json":
		analysis, err := AnalyzeStorage(ctx)
		if err != nil {
			logc.Fatalln(ctx, err)
		}

		encoder := json.NewEncoder(os.Stdout)
		encoder.Encode(analysis)

	case *analyzeStorage != "":
		logc.Fatalf(ctx, "unsupported -analyze-storage mode")

	case *traceGarbage:
		if err = TraceGarbage(ctx); err != nil {
			logc.Fatalln(ctx, err)
		}

	default:
		// Start listening on all ports before initializing the backend, otherwise if the backend
		// spends some time initializing (which the S3 backend does) a proxy like Caddy can race
		// with git-pages on startup and return errors for requests that would have been served
		// just 0.5s later.
		pagesListener := listen(ctx, "pages", config.Server.Pages)
		caddyListener := listen(ctx, "caddy", config.Server.Caddy)
		metricsListener := listen(ctx, "metrics", config.Server.Metrics)

		if backend, err = CreateBackend(ctx, &config.Storage); err != nil {
			logc.Fatalln(ctx, err)
		}
		backend = NewObservedBackend(backend)

		if existenceCache, err = CreateExistenceCache(ctx); err != nil {
			logc.Fatalln(ctx, err)
		}

		middleware := chainHTTPMiddleware(
			panicHandler,
			remoteAddrMiddleware,
			ObserveHTTPHandler,
		)
		go serve(ctx, pagesListener, middleware(http.HandlerFunc(ServePages)))
		go serve(ctx, caddyListener, middleware(http.HandlerFunc(ServeCaddy)))
		go serve(ctx, metricsListener, promhttp.Handler())

		if config.Insecure {
			logc.Println(ctx, "serve: ready (INSECURE)")
		} else {
			logc.Println(ctx, "serve: ready")
		}

		sys.WaitForInterrupt()
		logc.Println(ctx, "serve: exiting")
	}
}
