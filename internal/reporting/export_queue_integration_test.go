//go:build integration

// Queue-routing check for scheduled report exports — FR-RPT-002 | MDS §4.8.
//
// Split out from export_test.go and tagged: asserting where a task actually
// lands means enqueueing it through the real client, which needs the real
// queue tables (migration 037). The rest of the export tests are pure
// rendering and stay in the untagged file so they keep running in the
// default suite.
//
// Run: ./scripts/run_db_tests.sh, which sets TEST_DB_DSN
package reporting

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maaransoft/isp-bss-oss/internal/jobqueue"
)

// TestFR_RPT_002_ExportRunsOnItsOwnQueue — a ten-year aggregate is the slowest
// thing the worker pool runs, and a CoA queued behind one leaves a subscriber
// unthrottled for its duration.
func TestFR_RPT_002_ExportRunsOnItsOwnQueue(t *testing.T) {
	task, err := NewExportTask(ExportPayload{Report: ReportGrowth, EntityID: 1})
	if err != nil {
		t.Fatalf("NewExportTask: %v", err)
	}

	dsn := os.Getenv("TEST_DB_DSN")
	if dsn == "" {
		t.Skip("TEST_DB_DSN not set — run via ./scripts/run_db_tests.sh")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `TRUNCATE TABLE jobqueue_tasks RESTART IDENTITY`); err != nil {
		t.Fatalf("truncate jobqueue_tasks: %v", err)
	}

	// Enqueued through a real client rather than inspecting the task's options,
	// so what is asserted is where the task actually lands.
	client := jobqueue.NewClient(pool)
	defer client.Close() //nolint:errcheck
	inspector := jobqueue.NewInspector(pool)

	info, err := client.EnqueueContext(ctx, task)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if info.Queue != QueueReports {
		t.Errorf("exports must run on the %q queue, got %q — a ten-year aggregate ahead of a "+
			"CoA leaves a subscriber unthrottled for its duration", QueueReports, info.Queue)
	}

	pending, err := inspector.ListPending(ctx, QueueReports)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(pending) != 1 || pending[0].Type != TaskTypeReportExport {
		t.Errorf("want one %s task on the reports queue, got %+v", TaskTypeReportExport, pending)
	}
}
