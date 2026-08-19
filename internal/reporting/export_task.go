package reporting

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/jobqueue"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog/log"
)

// Scheduled report export — FR-RPT-002 | MDS §4.8.
//
// The asynchronous half. A synchronous download is right for a dashboard, and
// wrong for a ten-year collection report an owner wants waiting on Monday
// morning: it holds an HTTP connection open across a long query, and a browser
// timeout loses work already done.
//
// Delivery is into the archival store built for FR-DOC-001, which is why that
// schema already has a 'report' doc kind. The export therefore inherits a
// checksum and a retention date rather than accumulating CSV files nobody owns.

const (
	// TaskTypeReportExport is the task type for a scheduled export.
	TaskTypeReportExport = "report:export"
	// QueueReports keeps long report queries off the queue that carries
	// network commands: a CoA waiting behind a ten-year aggregate is a
	// subscriber left unthrottled for the duration.
	QueueReports = "reports"
)

var (
	exportsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "report_export_total",
		Help: "Scheduled report exports, by report and outcome",
	}, []string{"report", "outcome"})
	exportDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "report_export_duration_seconds",
		Help:    "Time to generate and deliver a scheduled report export",
		Buckets: []float64{0.1, 0.5, 1, 5, 15, 60, 300},
	}, []string{"report"})
)

// ExportPayload is the task payload.
type ExportPayload struct {
	Report string `json:"report"`
	Months int    `json:"months,omitempty"`
	// FranchiseID is resolved at enqueue time from the requesting token, not
	// read from the request, so a scheduled export cannot widen its own scope
	// between being asked for and being run.
	FranchiseID *int `json:"franchise_id,omitempty"`
	// RequestedBy records who asked, for the archive's audit trail.
	RequestedBy string `json:"requested_by,omitempty"`
	// EntityID keys the archive row. Exports have no natural entity, so the
	// enqueuer assigns one and hands it back to the caller as the export id.
	EntityID int `json:"entity_id"`
}

// Archiver delivers a generated report. Satisfied by *archive.Archiver.
//
// Declared as an interface here rather than importing the concrete type so
// this package does not depend on the storage backend, and so a deployment
// with archival switched off simply has no export worker rather than a broken
// one.
type Archiver interface {
	ArchiveReport(ctx context.Context, entityID int, filename string, body []byte) (string, error)
}

// ExportHandler generates a report and hands it to the archiver.
type ExportHandler struct {
	db       Querier
	archiver Archiver
}

// NewExportHandler constructs an ExportHandler.
func NewExportHandler(q Querier, a Archiver) *ExportHandler {
	return &ExportHandler{db: q, archiver: a}
}

// ProcessTask implements jobqueue.Handler.
//
// The CSV is built in memory before archival rather than streamed, because the
// archive needs its length and checksum up front and a report bounded at ten
// years of monthly aggregates is measured in megabytes. That trade would be
// wrong for a per-subscriber extract, which is not what this path serves.
func (h *ExportHandler) ProcessTask(ctx context.Context, t *jobqueue.Task) error {
	var p ExportPayload
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		// Undecodable payloads can never succeed, so retrying only fills the
		// dead-letter queue more slowly.
		return fmt.Errorf("reporting: unmarshal export payload: %w, %w", err, jobqueue.SkipRetry)
	}
	if !ValidReport(p.Report) {
		return fmt.Errorf("reporting: unknown report %q: %w", p.Report, jobqueue.SkipRetry)
	}

	timer := prometheus.NewTimer(exportDuration.WithLabelValues(p.Report))
	defer timer.ObserveDuration()

	filter := Filter{Months: p.Months, FranchiseID: p.FranchiseID}
	var buf bytes.Buffer
	if err := WriteCSV(ctx, h.db, p.Report, filter, &buf); err != nil {
		exportsTotal.WithLabelValues(p.Report, "generate_error").Inc()
		return fmt.Errorf("reporting: generate %s export: %w", p.Report, err)
	}

	if h.archiver == nil {
		exportsTotal.WithLabelValues(p.Report, "no_archiver").Inc()
		return fmt.Errorf("reporting: no archival backend configured: %w", jobqueue.SkipRetry)
	}

	url, err := h.archiver.ArchiveReport(ctx, p.EntityID,
		Filename(p.Report, filter, time.Now()), buf.Bytes())
	if err != nil {
		exportsTotal.WithLabelValues(p.Report, "deliver_error").Inc()
		return fmt.Errorf("reporting: deliver %s export: %w", p.Report, err)
	}

	exportsTotal.WithLabelValues(p.Report, "delivered").Inc()
	log.Info().Str("report", p.Report).Int("export_id", p.EntityID).
		Str("requested_by", p.RequestedBy).Str("url", url).
		Msg("reporting: scheduled export delivered")
	return nil
}

// NewExportTask builds the task for an export request.
func NewExportTask(p ExportPayload) (*jobqueue.Task, error) {
	payload, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("reporting: marshal export payload: %w", err)
	}
	return jobqueue.NewTask(TaskTypeReportExport, payload,
		jobqueue.Queue(QueueReports),
		jobqueue.MaxRetry(3),
		// Retained so an operator can see a finished export in the queue
		// inspector rather than only its archive row.
		jobqueue.Retention(48*time.Hour)), nil
}
