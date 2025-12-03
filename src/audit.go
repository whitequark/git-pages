package git_pages

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/influxdata/influxdb/pkg/snowflake"
	exponential "github.com/jpillora/backoff"
	"google.golang.org/protobuf/proto"
	timestamppb "google.golang.org/protobuf/types/known/timestamppb"
)

func EncodeAuditRecord(auditRecord *AuditRecord) (data []byte) {
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(auditRecord)
	if err != nil {
		panic(err)
	}
	return
}

func DecodeAuditRecord(data []byte) (auditRecord *AuditRecord, err error) {
	auditRecord = &AuditRecord{}
	err = proto.Unmarshal(data, auditRecord)
	return
}

type auditedBackend struct {
	Backend
	ids *snowflake.Generator
}

var _ Backend = (*auditedBackend)(nil)

func NewAuditedBackend(backend Backend) Backend {
	if config.Feature("audit") {
		ids := snowflake.New(config.Audit.NodeID)
		return &auditedBackend{backend, ids}
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
	record.Timestamp = timestamppb.Now()

	if config.Audit.Collect {
		id := fmt.Sprintf("%016x", audited.ids.Next())
		err = audited.Backend.AppendAuditRecord(ctx, id, record)
		if err != nil {
			err = fmt.Errorf("audit: %w", err)
		} else {
			var subject string
			if record.Project == nil {
				subject = *record.Domain
			} else {
				subject = fmt.Sprintf("%s/%s", *record.Domain, *record.Project)
			}
			logc.Printf(ctx, "audit %s ok: %s %s\n", subject, record.Event.String(), id)

			// Send a notification to the audit server, if configured, and try to make sure
			// it is delivered by retrying with exponential backoff on errors.
			notifyAudit(context.WithoutCancel(ctx), id)
		}
	}
	return
}

func notifyAudit(ctx context.Context, id string) {
	if config.Audit.NotifyURL != nil {
		notifyURL := config.Audit.NotifyURL.URL
		notifyURL.RawQuery = id
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
					time.Sleep(sleepFor)
				} else {
					logc.Printf(ctx, "audit notify %s ok", id)
					break
				}
			}
		}()
	}
}

func (audited *auditedBackend) CommitManifest(ctx context.Context, name string, manifest *Manifest) (err error) {
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

	return audited.Backend.CommitManifest(ctx, name, manifest)
}

func (audited *auditedBackend) DeleteManifest(ctx context.Context, name string) (err error) {
	domain, project, ok := strings.Cut(name, "/")
	if !ok {
		panic("malformed manifest name")
	}
	audited.appendNewAuditRecord(ctx, &AuditRecord{
		Event:   AuditEvent_DeleteManifest.Enum(),
		Domain:  proto.String(domain),
		Project: proto.String(project),
	})

	return audited.Backend.DeleteManifest(ctx, name)
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
