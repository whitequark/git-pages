package git_pages

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	exponential "github.com/jpillora/backoff"
	"github.com/kankanreno/go-snowflake"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	timestamppb "google.golang.org/protobuf/types/known/timestamppb"
)

var (
	auditNotifyOkCount = promauto.NewCounter(prometheus.CounterOpts{
		Name: "git_pages_audit_notify_ok",
		Help: "Count of successful audit notifications",
	})
	auditNotifyErrorCount = promauto.NewCounter(prometheus.CounterOpts{
		Name: "git_pages_audit_notify_error",
		Help: "Count of failed audit notifications",
	})
)

type principalKey struct{}

var PrincipalKey = principalKey{}

func WithPrincipal(ctx context.Context) context.Context {
	principal := &Principal{}
	return context.WithValue(ctx, PrincipalKey, principal)
}

func GetPrincipal(ctx context.Context) *Principal {
	if principal, ok := ctx.Value(PrincipalKey).(*Principal); ok {
		return principal
	}
	return nil
}

type AuditID int64

func GenerateAuditID() AuditID {
	inner, err := snowflake.NextID()
	if err != nil {
		panic(err)
	}
	return AuditID(inner)
}

func ParseAuditID(repr string) (AuditID, error) {
	inner, err := strconv.ParseInt(repr, 16, 64)
	if err != nil {
		return AuditID(0), err
	}
	return AuditID(inner), nil
}

func (id AuditID) String() string {
	return fmt.Sprintf("%016x", int64(id))
}

func (id AuditID) CompareTime(when time.Time) int {
	idMillis := int64(id) >> (snowflake.MachineIDLength + snowflake.SequenceLength)
	whenMillis := when.UTC().UnixNano() / 1e6
	return cmp.Compare(idMillis, whenMillis)
}

func EncodeAuditRecord(record *AuditRecord) (data []byte) {
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(record)
	if err != nil {
		panic(err)
	}
	return
}

func DecodeAuditRecord(data []byte) (record *AuditRecord, err error) {
	record = &AuditRecord{}
	err = proto.Unmarshal(data, record)
	return
}

func (record *AuditRecord) GetAuditID() AuditID {
	return AuditID(record.GetId())
}

func (record *AuditRecord) DescribePrincipal() string {
	var items []string
	if record.Principal != nil {
		if record.Principal.GetIpAddress() != "" {
			items = append(items, record.Principal.GetIpAddress())
		}
		if record.Principal.GetCliAdmin() {
			items = append(items, "<cli-admin>")
		}
	}
	if len(items) > 0 {
		return strings.Join(items, ";")
	} else {
		return "<unknown>"
	}
}

func (record *AuditRecord) DescribeResource() string {
	desc := "<unknown>"
	if record.Domain != nil && record.Project != nil {
		desc = path.Join(*record.Domain, *record.Project)
	} else if record.Domain != nil {
		desc = *record.Domain
	}
	return desc
}

type AuditRecordScope int

const (
	AuditRecordComplete AuditRecordScope = iota
	AuditRecordNoManifest
)

func AuditRecordJSON(record *AuditRecord, scope AuditRecordScope) []byte {
	switch scope {
	case AuditRecordComplete:
		// as-is
	case AuditRecordNoManifest:
		// trim the manifest
		newRecord := &AuditRecord{}
		proto.Merge(newRecord, record)
		newRecord.Manifest = nil
		record = newRecord
	}

	json, err := protojson.MarshalOptions{
		Multiline:         true,
		EmitDefaultValues: true,
	}.Marshal(record)
	if err != nil {
		panic(err)
	}
	return json
}

// This function receives `id` and `record` separately because the record itself may have its
// ID missing or mismatched. While this is very unlikely, using the actual primary key as
// the filename is more robust.
func ExtractAuditRecord(ctx context.Context, id AuditID, record *AuditRecord, dest string) error {
	const mode = 0o400 // readable by current user, not writable

	err := os.WriteFile(filepath.Join(dest, fmt.Sprintf("%s-event.json", id)),
		AuditRecordJSON(record, AuditRecordNoManifest), mode)
	if err != nil {
		return err
	}

	if record.Manifest != nil {
		err = os.WriteFile(filepath.Join(dest, fmt.Sprintf("%s-manifest.json", id)),
			ManifestJSON(record.Manifest), mode)
		if err != nil {
			return err
		}

		archive, err := os.OpenFile(filepath.Join(dest, fmt.Sprintf("%s-archive.tar", id)),
			os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
		if err != nil {
			return err
		}
		defer archive.Close()

		err = CollectTar(ctx, archive, record.Manifest, ManifestMetadata{})
		if err != nil {
			return err
		}
	}

	return nil
}

func AuditEventProcessor(command string, args []string) (http.Handler, error) {
	var err error

	// Resolve the command to an absolute path, as it will be run from a different current
	// directory, which would break e.g. `git-pages -audit-server tcp/:3004 ./handler.sh`.
	if command, err = exec.LookPath(command); err != nil {
		return nil, err
	}
	if command, err = filepath.Abs(command); err != nil {
		return nil, err
	}

	router := http.NewServeMux()
	router.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Go will cancel the request context if the client drops the connection. We don't want
		// that to interrupt processing. However, we also want the client (not the server) to
		// handle retries, so instead of spawning a goroutine to process the event, we do this
		// within the HTTP handler. If an error is returned, the notify goroutine in the worker
		// will retry the HTTP request (with backoff) until it succeeds.
		//
		// This is a somewhat idiosyncratic design and it's not clear that this is the best
		// possible approach (e.g. if the worker gets restarted and the event processing fails,
		// it will not be retried), but it should do the job for now. It is expected that
		// some form of observability is used to highlight event processor errors.
		ctx := context.WithoutCancel(r.Context())

		id, err := ParseAuditID(r.URL.RawQuery)
		if err != nil {
			logc.Printf(ctx, "audit process err: malformed query\n")
			http.Error(w, "malformed query", http.StatusBadRequest)
			return
		} else {
			logc.Printf(ctx, "audit process %s", id)
		}

		record, err := backend.QueryAuditLog(ctx, id)
		if err != nil {
			logc.Printf(ctx, "audit process err: missing record\n")
			http.Error(w, "missing record", http.StatusNotFound)
			return
		}

		args := append(args, id.String(), record.GetEvent().String())
		cmd := exec.CommandContext(ctx, command, args...)
		if cmd.Dir, err = os.MkdirTemp("", "auditRecord"); err != nil {
			panic(fmt.Errorf("mkdtemp: %w", err))
		}
		defer os.RemoveAll(cmd.Dir)

		if err = ExtractAuditRecord(ctx, id, record, cmd.Dir); err != nil {
			logc.Printf(ctx, "audit process %s err: %s\n", id, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		output, err := cmd.CombinedOutput()
		if err != nil {
			logc.Printf(ctx, "audit process %s err: %s; %s\n", id, err, string(output))
			w.WriteHeader(http.StatusServiceUnavailable)
			if len(output) == 0 {
				fmt.Fprintln(w, err.Error())
			}
		} else {
			logc.Printf(ctx, "audit process %s ok: %s\n", id, string(output))
			w.WriteHeader(http.StatusOK)
		}
		w.Write(output)
	}))
	return router, nil
}

type auditedBackend struct {
	Backend
}

var _ Backend = (*auditedBackend)(nil)

func NewAuditedBackend(backend Backend) Backend {
	if config.Feature("audit") {
		return &auditedBackend{backend}
	} else {
		return backend
	}
}

// This function does not retry appending audit records; as such, if it returns an error,
// this error must interrupt whatever operation it was auditing. A corollary is that it is
// possible that appending an audit record succeeds but the audited operation fails.
// This is considered fine since the purpose of auditing is to record end user intent, not
// to be a 100% accurate reflection of performed actions. When in doubt, the audit records
// should be examined together with the application logs.
func (audited *auditedBackend) appendNewAuditRecord(ctx context.Context, record *AuditRecord) (err error) {
	if config.Audit.Collect {
		id := GenerateAuditID()
		record.Id = proto.Int64(int64(id))
		record.Timestamp = timestamppb.Now()
		record.Principal = GetPrincipal(ctx)

		err = audited.Backend.AppendAuditLog(ctx, id, record)
		if err != nil {
			err = fmt.Errorf("audit: %w", err)
		} else {
			var subject string
			if record.Project == nil {
				subject = *record.Domain
			} else {
				subject = path.Join(*record.Domain, *record.Project)
			}
			logc.Printf(ctx, "audit %s ok: %s %s\n", subject, id, record.Event.String())

			// Send a notification to the audit server, if configured, and try to make sure
			// it is delivered by retrying with exponential backoff on errors.
			notifyAudit(context.WithoutCancel(ctx), id)
		}
	}
	return
}

func notifyAudit(ctx context.Context, id AuditID) {
	if config.Audit.NotifyURL != nil {
		notifyURL := config.Audit.NotifyURL.URL
		notifyURL.RawQuery = id.String()

		// See also the explanation in `AuditEventProcessor` above.
		go func() {
			backoff := exponential.Backoff{
				Jitter: true,
				Min:    time.Second * 1,
				Max:    time.Second * 60,
			}
			for {
				resp, err := http.Get(notifyURL.String())
				var body []byte
				if err == nil {
					defer resp.Body.Close()
					body, _ = io.ReadAll(resp.Body)
				}
				if err == nil && resp.StatusCode == http.StatusOK {
					logc.Printf(ctx, "audit notify %s ok: %s\n", id, string(body))
					auditNotifyOkCount.Inc()
					break
				} else {
					sleepFor := backoff.Duration()
					if err != nil {
						logc.Printf(ctx, "audit notify %s err: %s (retry in %s)",
							id, err, sleepFor)
					} else {
						logc.Printf(ctx, "audit notify %s fail: %s (retry in %s); %s",
							id, resp.Status, sleepFor, string(body))
					}
					auditNotifyErrorCount.Inc()
					time.Sleep(sleepFor)
				}
			}
		}()
	}
}

func (audited *auditedBackend) CommitManifest(
	ctx context.Context, name string, manifest *Manifest, opts ModifyManifestOptions,
) (err error) {
	domain, project, ok := strings.Cut(name, "/")
	if !ok {
		panic("malformed manifest name")
	}
	audited.appendNewAuditRecord(ctx, &AuditRecord{
		Event:    AuditEvent_CommitManifest.Enum(),
		Domain:   proto.String(domain),
		Project:  proto.String(project),
		Manifest: manifest,
	})

	return audited.Backend.CommitManifest(ctx, name, manifest, opts)
}

func (audited *auditedBackend) DeleteManifest(
	ctx context.Context, name string, opts ModifyManifestOptions,
) (err error) {
	domain, project, ok := strings.Cut(name, "/")
	if !ok {
		panic("malformed manifest name")
	}
	audited.appendNewAuditRecord(ctx, &AuditRecord{
		Event:   AuditEvent_DeleteManifest.Enum(),
		Domain:  proto.String(domain),
		Project: proto.String(project),
	})

	return audited.Backend.DeleteManifest(ctx, name, opts)
}

func (audited *auditedBackend) FreezeDomain(ctx context.Context, domain string, freeze bool) (err error) {
	var event AuditEvent
	if freeze {
		event = AuditEvent_FreezeDomain
	} else {
		event = AuditEvent_UnfreezeDomain
	}
	audited.appendNewAuditRecord(ctx, &AuditRecord{
		Event:  event.Enum(),
		Domain: proto.String(domain),
	})

	return audited.Backend.FreezeDomain(ctx, domain, freeze)
}
