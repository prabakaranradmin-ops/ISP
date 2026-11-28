//go:build integration

package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/radius"
)

// The device behind a session, against a real database — FR-HSP-002,
// FR-AAA-003 | migration 077.
//
// Deactivating a registered hotspot phone used to stop its next login and
// leave the session it already had, because nothing recorded which session
// was the phone's.
func TestFR_HSP_002_ADevicesLiveSessionsAreFoundByItsMAC(t *testing.T) {
	ctx := context.Background()
	d, pool := newTestDB(t)
	seedPlan(ctx, t, pool, 1, "Home 50", "50M/50M", 0, "", "500.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "kavya", MobileNumber: "+919820000201"})
	s := d.FUP()
	const phone = "AA:BB:CC:DD:EE:01"

	for _, st := range []radius.SessionStart{
		{SubscriberID: 1, SessionID: "phone-now", NASIP: "10.0.0.5", DeviceMAC: phone, MAB: true},
		{SubscriberID: 1, SessionID: "phone-earlier", NASIP: "10.0.0.5", DeviceMAC: phone, MAB: true},
		// The same MAC behind a PPPoE login is a router, not the hotspot
		// registration; deactivating the phone says nothing about it.
		{SubscriberID: 1, SessionID: "router", NASIP: "10.0.0.1", DeviceMAC: phone},
		{SubscriberID: 1, SessionID: "other-phone", NASIP: "10.0.0.5", DeviceMAC: "AA:BB:CC:DD:EE:02", MAB: true},
		{SubscriberID: 1, SessionID: "no-device", NASIP: "10.0.0.1"},
	} {
		if err := s.OpenSession(ctx, st); err != nil {
			t.Fatalf("open %s: %v", st.SessionID, err)
		}
	}
	if _, err := s.StopSession(ctx, "phone-earlier", 0, 0, "User-Request"); err != nil {
		t.Fatal(err)
	}

	got, err := d.Hotspot().LiveDeviceSessions(ctx, phone)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SessionID != "phone-now" || got[0].NasIP != "10.0.0.5" {
		t.Errorf("live sessions for the phone %+v; want only its open MAB session", got)
	}

	// What was recorded, read back.
	var mac *string
	var mab bool
	if err := pool.QueryRow(ctx,
		`SELECT device_mac, mab FROM subscriber_session_history WHERE session_id = 'router'`).Scan(&mac, &mab); err != nil {
		t.Fatal(err)
	}
	if mac == nil || *mac != phone || mab {
		t.Errorf("the router session recorded %v mab=%v", mac, mab)
	}
	if err := pool.QueryRow(ctx,
		`SELECT device_mac, mab FROM subscriber_session_history WHERE session_id = 'no-device'`).Scan(&mac, &mab); err != nil {
		t.Fatal(err)
	}
	if mac != nil || mab {
		t.Errorf("a session with no device recorded %v mab=%v; it must be NULL, not empty", mac, mab)
	}

	// The column holds the normalised form only, so a lookup by MAC cannot
	// miss a row stored in some other spelling.
	if err := s.OpenSession(ctx, radius.SessionStart{SubscriberID: 1, SessionID: "lower",
		NASIP: "10.0.0.5", DeviceMAC: "aa:bb:cc:dd:ee:01"}); err == nil {
		t.Error("a lower-case MAC was stored; chk_session_device_mac must refuse it")
	}
}

// A session the NAS says does not exist is closed, with a cause that says
// how; one that is already closed keeps its own.
func TestFR_AAA_003_ASessionTheNASDisownsIsClosedInHistory(t *testing.T) {
	ctx := context.Background()
	d, pool := newTestDB(t)
	seedPlan(ctx, t, pool, 1, "Home 50", "50M/50M", 0, "", "500.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "ravi", MobileNumber: "+919820000202"})
	s := d.FUP()
	for _, id := range []string{"stale", "ended"} {
		if err := s.StartSession(ctx, 1, id, "10.0.0.1", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.StopSession(ctx, "ended", 10, 10, "User-Request"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE subscriber_session_history SET stop_time = now() - interval '2 hours' WHERE session_id = 'ended'`); err != nil {
		t.Fatal(err)
	}
	var endedAt time.Time
	if err := pool.QueryRow(ctx,
		`SELECT stop_time FROM subscriber_session_history WHERE session_id = 'ended'`).Scan(&endedAt); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"stale", "ended", "never-existed"} {
		if err := s.CloseGoneSession(ctx, id); err != nil {
			t.Fatalf("close %s: %v", id, err)
		}
	}
	open, err := s.ListSubscriberNASSessions(ctx, 1)
	if err != nil || len(open) != 0 {
		t.Errorf("open sessions after closing %+v %v", open, err)
	}
	var cause string
	if err := pool.QueryRow(ctx,
		`SELECT terminate_cause FROM subscriber_session_history WHERE session_id = 'stale'`).Scan(&cause); err != nil {
		t.Fatal(err)
	}
	if cause != "nas-reports-no-session" {
		t.Errorf("stale session closed with cause %q", cause)
	}
	var stoppedAt time.Time
	if err := pool.QueryRow(ctx,
		`SELECT terminate_cause, stop_time FROM subscriber_session_history WHERE session_id = 'ended'`).Scan(&cause, &stoppedAt); err != nil {
		t.Fatal(err)
	}
	if cause != "User-Request" || !stoppedAt.Equal(endedAt) {
		t.Errorf("an already-closed session was rewritten: cause %q, stopped %v (was %v)", cause, stoppedAt, endedAt)
	}
}
