//go:build integration

// RADIUS authentication attempt log — FR-NET-004 | migration 063.
//
// The claims worth proving against a real database: the batch write lands
// with every column intact, the vocabulary CHECKs actually refuse what is
// outside them, retention deletes only what is expired, and — the one the
// requirement is explicit about — there is nowhere in the table to put a
// password.
package db_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/radius"
)

func attempt(username, result, reason, method string) radius.AuthAttempt {
	return radius.AuthAttempt{
		Username: username, NASIP: "198.51.100.1", MAC: "AA:BB:CC:DD:EE:FF",
		Result: result, RejectReason: reason, AuthMethod: method, At: time.Now(),
	}
}

func TestFR_NET_004_AttemptsAreWrittenAndReadBack(t *testing.T) {
	ctx := context.Background()
	database, _ := newTestDB(t)
	store := database.AuthLog()

	batch := []radius.AuthAttempt{
		attempt("acme", radius.ResultReject, radius.RejectBadPassword, radius.MethodPAP),
		attempt("acme", radius.ResultAccept, "", radius.MethodPAP),
		attempt("ghost", radius.ResultReject, radius.RejectUnknownUser, radius.MethodEAP),
	}
	if err := store.RecordAuthAttempts(ctx, batch); err != nil {
		t.Fatalf("record batch: %v", err)
	}

	got, err := store.ListAuthAttempts(ctx, radius.AuthLogFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 attempts, got %d", len(got))
	}

	var reject *radius.AuthLogEntry
	for i := range got {
		if got[i].Username == "acme" && got[i].Result == radius.ResultReject {
			reject = &got[i]
		}
	}
	if reject == nil {
		t.Fatal("the rejected attempt is missing")
	}
	if reject.RejectReason != radius.RejectBadPassword || reject.AuthMethod != radius.MethodPAP {
		t.Errorf("attempt reads %+v", reject)
	}
	// host() strips the mask INET carries, so the NAS address reads back
	// as an address rather than 198.51.100.1/32.
	if reject.NASIP != "198.51.100.1" {
		t.Errorf("nas_ip read back as %q", reject.NASIP)
	}
	if reject.MAC != "AA:BB:CC:DD:EE:FF" {
		t.Errorf("mac read back as %q", reject.MAC)
	}
	if reject.Accepted() {
		t.Error("a reject reports itself accepted")
	}

	t.Run("an empty batch is not an error", func(t *testing.T) {
		if err := store.RecordAuthAttempts(ctx, nil); err != nil {
			t.Errorf("an empty batch failed: %v", err)
		}
	})

	t.Run("an attempt with no NAS address is accepted", func(t *testing.T) {
		// nas_ip is INET and will not take an empty string; the store
		// passes NULL instead. A NAS that did not identify itself is
		// still an attempt worth recording.
		a := attempt("nonas", radius.ResultReject, radius.RejectUnknownUser, radius.MethodPAP)
		a.NASIP = ""
		if err := store.RecordAuthAttempts(ctx, []radius.AuthAttempt{a}); err != nil {
			t.Fatalf("an attempt with no NAS address was refused: %v", err)
		}
		got, err := store.ListAuthAttempts(ctx, radius.AuthLogFilter{Username: "nonas"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].NASIP != "" {
			t.Errorf("read back as %+v", got)
		}
	})

	t.Run("a bad result does not lose the whole batch", func(t *testing.T) {
		// One malformed row would otherwise fail the CHECK and take every
		// other attempt in the batch with it. The store normalises it to
		// a reject with internal_error, which is true and findable.
		bad := attempt("weird", "sideways", "", radius.MethodPAP)
		ok := attempt("fine", radius.ResultAccept, "", radius.MethodPAP)
		if err := store.RecordAuthAttempts(ctx, []radius.AuthAttempt{bad, ok}); err != nil {
			t.Fatalf("a batch with one bad row failed entirely: %v", err)
		}
		got, err := store.ListAuthAttempts(ctx, radius.AuthLogFilter{Username: "weird"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Result != radius.ResultReject ||
			got[0].RejectReason != radius.RejectInternalError {
			t.Errorf("the malformed row stored as %+v", got)
		}
		// And the good row alongside it survived.
		fine, err := store.ListAuthAttempts(ctx, radius.AuthLogFilter{Username: "fine"})
		if err != nil || len(fine) != 1 {
			t.Errorf("the good row in the same batch was lost: %v %d", err, len(fine))
		}
	})

	t.Run("a very long username is truncated rather than refused", func(t *testing.T) {
		long := strings.Repeat("x", 400)
		if err := store.RecordAuthAttempts(ctx, []radius.AuthAttempt{
			attempt(long, radius.ResultReject, radius.RejectUnknownUser, radius.MethodPAP)}); err != nil {
			t.Fatalf("a long username failed the batch: %v", err)
		}
	})
}

// The table has nowhere to put a password. Asserted against the real
// schema, not the Go struct: a column could be added by a later migration
// without touching AuthAttempt at all.
func TestFR_NET_004_TheTableHasNoColumnForACredential(t *testing.T) {
	ctx := context.Background()
	_, pool := newTestDB(t)

	rows, err := pool.Query(ctx, `
		SELECT column_name FROM information_schema.columns
		 WHERE table_name = 'radius_auth_log'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	forbidden := []string{"password", "passwd", "secret", "credential", "pwd", "hash"}
	var columns []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, c)
		lower := strings.ToLower(c)
		for _, bad := range forbidden {
			if strings.Contains(lower, bad) {
				t.Errorf("radius_auth_log has a column %q: the authentication log must never store a credential (FR-NET-004)", c)
			}
		}
	}
	if len(columns) == 0 {
		t.Fatal("radius_auth_log has no columns; migration 063 did not run")
	}
}

// The CHECKs refuse what is outside the vocabulary, so a typo in a reason
// fails loudly here rather than becoming an ungroupable value.
func TestFR_NET_004_TheSchemaRefusesAnUnknownVocabulary(t *testing.T) {
	ctx := context.Background()
	_, pool := newTestDB(t)

	cases := []struct {
		name string
		sql  string
	}{
		{"an unknown result", `INSERT INTO radius_auth_log (username, result) VALUES ('x', 'maybe')`},
		{"an unknown reject reason", `INSERT INTO radius_auth_log (username, result, reject_reason)
			VALUES ('x', 'reject', 'because')`},
		{"an unknown auth method", `INSERT INTO radius_auth_log (username, result, reject_reason, auth_method)
			VALUES ('x', 'reject', 'bad_password', 'telepathy')`},
		{"a reject with no reason", `INSERT INTO radius_auth_log (username, result)
			VALUES ('x', 'reject')`},
		{"an accept carrying a reason", `INSERT INTO radius_auth_log (username, result, reject_reason)
			VALUES ('x', 'accept', 'bad_password')`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := pool.Exec(ctx, tc.sql); err == nil {
				t.Error("the database accepted it")
			}
		})
	}
}

// Retention deletes what is expired and nothing else.
func TestFR_NET_004_RetentionPurgesOnlyExpiredAttempts(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.AuthLog()

	if err := store.RecordAuthAttempts(ctx, []radius.AuthAttempt{
		attempt("recent", radius.ResultAccept, "", radius.MethodPAP)}); err != nil {
		t.Fatal(err)
	}
	// Two rows well past the window.
	for i := 0; i < 2; i++ {
		if _, err := pool.Exec(ctx, `
			INSERT INTO radius_auth_log (username, result, auth_method, created_at)
			VALUES ('ancient', 'accept', 'pap', now() - interval '40 days')`); err != nil {
			t.Fatal(err)
		}
	}

	deleted, err := store.PurgeAuthLog(ctx, radius.AuthLogRetention, 1000)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if deleted != 2 {
		t.Errorf("purged %d rows, want the 2 expired ones", deleted)
	}

	left, err := store.ListAuthAttempts(ctx, radius.AuthLogFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0].Username != "recent" {
		t.Errorf("the purge removed the wrong rows: %+v", left)
	}

	t.Run("a second pass with nothing expired deletes nothing", func(t *testing.T) {
		n, err := store.PurgeAuthLog(ctx, radius.AuthLogRetention, 1000)
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("a purge with nothing to do removed %d rows", n)
		}
	})

	t.Run("the batch limit bounds one call", func(t *testing.T) {
		for i := 0; i < 5; i++ {
			if _, err := pool.Exec(ctx, `
				INSERT INTO radius_auth_log (username, result, auth_method, created_at)
				VALUES ('old', 'accept', 'pap', now() - interval '40 days')`); err != nil {
				t.Fatal(err)
			}
		}
		n, err := store.PurgeAuthLog(ctx, radius.AuthLogRetention, 2)
		if err != nil {
			t.Fatal(err)
		}
		if n != 2 {
			t.Errorf("a limit of 2 deleted %d rows; one pass must not hold a long transaction", n)
		}
	})
}

func TestFR_NET_004_FilteringAndSummary(t *testing.T) {
	ctx := context.Background()
	database, _ := newTestDB(t)
	store := database.AuthLog()

	if err := store.RecordAuthAttempts(ctx, []radius.AuthAttempt{
		attempt("acme", radius.ResultReject, radius.RejectBadPassword, radius.MethodPAP),
		attempt("acme", radius.ResultReject, radius.RejectBadPassword, radius.MethodPAP),
		attempt("acme", radius.ResultAccept, "", radius.MethodPAP),
		attempt("ghost", radius.ResultReject, radius.RejectUnknownUser, radius.MethodEAP),
	}); err != nil {
		t.Fatal(err)
	}

	t.Run("by username", func(t *testing.T) {
		got, err := store.ListAuthAttempts(ctx, radius.AuthLogFilter{Username: "ghost"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Errorf("got %d", len(got))
		}
	})

	t.Run("by result", func(t *testing.T) {
		got, err := store.ListAuthAttempts(ctx, radius.AuthLogFilter{Result: radius.ResultReject})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 3 {
			t.Errorf("got %d rejects, want 3", len(got))
		}
	})

	t.Run("by reason", func(t *testing.T) {
		got, err := store.ListAuthAttempts(ctx, radius.AuthLogFilter{Reason: radius.RejectBadPassword})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Errorf("got %d", len(got))
		}
	})

	// The first thing anybody looks at: which failure is happening, not
	// which attempts happened. "Forty bad_password" and "forty
	// not_authorised" are different incidents.
	t.Run("rejects summarise by reason, commonest first", func(t *testing.T) {
		since := time.Now().Add(-time.Hour)
		got, err := store.SummariseAuthRejects(ctx, since)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("want 2 reasons, got %+v", got)
		}
		if got[0].Reason != radius.RejectBadPassword || got[0].Count != 2 {
			t.Errorf("commonest reason is %+v, want bad_password x2", got[0])
		}
		// Accepts are not in the summary: it is a summary of failures.
		for _, r := range got {
			if r.Reason == "" {
				t.Error("an accept leaked into the reject summary")
			}
		}
	})
}
