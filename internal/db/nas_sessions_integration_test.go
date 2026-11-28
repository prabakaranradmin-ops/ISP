//go:build integration

package db_test

import (
	"context"
	"testing"
	"time"
)

// Every open session a subscriber holds, for the CoA and disconnect handlers
// — FR-FUP-002, FR-NET-002, FR-BIL-004.
//
// GetSubscriberNASSession returns the newest open session only, and the
// handlers used to stop there. A subscriber with a PPPoE line and a hotspot
// phone kept one of them through every throttle and suspension.
func TestFR_BIL_004_EveryOpenSessionIsListedNewestFirst(t *testing.T) {
	ctx := context.Background()
	d, pool := newTestDB(t)
	seedPlan(ctx, t, pool, 1, "Home 50", "50M/50M", 0, "", "500.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "two-lines", MobileNumber: "+919820000101"})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "someone-else", MobileNumber: "+919820000102"})
	s := d.FUP()

	for _, sess := range []struct {
		sub      int
		id, nas  string
		startAgo time.Duration
	}{
		{1, "pppoe-home", "10.0.0.1", 3 * time.Hour},
		{1, "hotspot-phone", "10.0.0.2", 10 * time.Minute},
		{1, "yesterday", "10.0.0.1", 26 * time.Hour},
		{2, "not-theirs", "10.0.0.1", time.Minute},
	} {
		if err := s.StartSession(ctx, sess.sub, sess.id, sess.nas, "", ""); err != nil {
			t.Fatalf("start %s: %v", sess.id, err)
		}
		if _, err := pool.Exec(ctx,
			`UPDATE subscriber_session_history SET start_time = now() - $2::interval WHERE session_id = $1`,
			sess.id, sess.startAgo.String()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.StopSession(ctx, "yesterday", 100, 100, "User-Request"); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListSubscriberNASSessions(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].SessionID != "hotspot-phone" || got[1].SessionID != "pppoe-home" {
		t.Fatalf("sessions %+v; want both open ones, newest first, and not the closed one or another subscriber's", got)
	}
	if got[0].NasIP != "10.0.0.2" || got[1].NasIP != "10.0.0.1" {
		t.Errorf("NAS addresses %+v; host() must strip the mask so the address can be dialled", got)
	}

	// The single-session lookup still agrees on which is newest.
	nasIP, sessionID, _, _, err := s.GetSubscriberNASSession(ctx, 1)
	if err != nil || sessionID != got[0].SessionID || nasIP != got[0].NasIP {
		t.Errorf("GetSubscriberNASSession %s@%s %v; the list's first entry must be the same session", sessionID, nasIP, err)
	}

	none, err := s.ListSubscriberNASSessions(ctx, 999)
	if err != nil || len(none) != 0 {
		t.Errorf("a subscriber with no sessions: %+v %v", none, err)
	}
}
