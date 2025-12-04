package git_pages

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
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
		desc = fmt.Sprintf("%s/%s", *record.Domain, *record.Project)
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
				subject = fmt.Sprintf("%s/%s", *record.Domain, *record.Project)
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

		go func() {
			backoff := exponential.Backoff{
				Jitter: true,
				Min:    time.Second * 1,
				Max:    time.Second * 60,
			}
			for {
				_, err := http.Get(notifyURL.String())
				if err != nil {
					sleepFor := backoff.Duration()
					logc.Printf(ctx, "audit notify %s err: %s (retry in %s)", id, err, sleepFor)
					auditNotifyErrorCount.Inc()
					time.Sleep(sleepFor)
				} else {
					logc.Printf(ctx, "audit notify %s ok", id)
					auditNotifyOkCount.Inc()
					break
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
