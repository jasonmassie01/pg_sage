package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/siem"
)

// Re-anchoring is offered only with sinks configured, and refuses
// distinguishably while the exporter is not running; with it running the
// request reaches the exporter (an unknown sink is its refusal).
func TestSIEMReanchorFunc(t *testing.T) {
	prev := auditExport.Load()
	t.Cleanup(func() { auditExport.Store(prev) })
	auditExport.Store(nil)
	if siemReanchorFunc(nil) != nil || siemReanchorFunc(config.DefaultConfig()) != nil {
		t.Fatalf("re-anchor offered without sinks")
	}
	c := config.DefaultConfig()
	c.Audit.SIEM.Sinks = []config.AuditSIEMSink{{Name: "soc", Type: "http"}}
	fn := siemReanchorFunc(c)
	if fn == nil {
		t.Fatalf("re-anchor not offered with a sink configured")
	}
	req := siem.ReanchorRequest{Sink: "nope", Source: "control", Chain: "action_log",
		FromSeq: 1}
	if _, err := fn(context.Background(), req); !errors.Is(err, errSIEMNotRunning) {
		t.Fatalf("exporter not running: err = %v", err)
	}
	auditExport.Store(siem.NewExporter(func() []siem.Source { return nil }, nil,
		siem.NewCursorStore(nil), siem.Options{Interval: time.Second},
		func() (siem.Fence, bool) { return siem.Fence{}, true }, nil))
	if _, err := fn(context.Background(), req); !errors.Is(err, siem.ErrUnknownSink) {
		t.Fatalf("running exporter: err = %v, want its unknown-sink refusal", err)
	}
}

// Verification sources carry the running exporter's cursors, and none
// while export is off.
func TestAuditSourcesCarryTheExportCursors(t *testing.T) {
	prev := auditExport.Load()
	t.Cleanup(func() { auditExport.Store(prev) })
	control := &pgxpool.Pool{}
	auditExport.Store(nil)
	if got := auditSources(control, nil)(); len(got) != 1 || got[0].Cursors != nil {
		t.Fatalf("export off: %+v", got)
	}
	e := siem.NewExporter(func() []siem.Source { return nil }, nil, siem.NewCursorStore(nil),
		siem.Options{Interval: time.Second}, func() (siem.Fence, bool) {
			return siem.Fence{}, true
		}, nil)
	auditExport.Store(e)
	got := auditSources(control, nil)()
	if len(got) != 1 || got[0].Cursors != e {
		t.Fatalf("export on: %+v", got)
	}
}
