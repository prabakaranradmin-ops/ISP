//go:build integration

package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/nastraffic"
)

// NAS interface traffic against a real database — FR-NET-006 | migration 073.
//
// What needs PostgreSQL: unsigned 64-bit counters surviving a NUMERIC round
// trip, the compare-and-swap that stops two pollers recording one interval
// twice, the time-weighted bucketing, and gaps staying gaps.

func i64p(v int64) *int64 { return &v }

func iface(name string, idx int, in, out uint64) nastraffic.Interface {
	up := true
	return nastraffic.Interface{Index: idx, Name: name, Type: 6, SpeedMbps: i64p(1000), OperUp: &up,
		HasHC: true, InOctets: in, OutOctets: out}
}

// pollAt runs one poll through the real store the way the poller does:
// read baselines, compute, save.
func pollAt(ctx context.Context, t *testing.T, store interface {
	InterfaceBaselines(context.Context, int) (map[string]nastraffic.Previous, error)
	SavePoll(context.Context, int, time.Time, []nastraffic.Write) error
}, nasID int, at time.Time, ifaces ...nastraffic.Interface) {
	t.Helper()
	prev, err := store.InterfaceBaselines(ctx, nasID)
	if err != nil {
		t.Fatalf("baselines: %v", err)
	}
	var writes []nastraffic.Write
	for _, i := range ifaces {
		var expect *nastraffic.Previous
		if b, ok := prev[i.Name]; ok {
			expect = &b
		}
		s, o := nastraffic.Rate(expect, i, at)
		w := nastraffic.Write{Iface: i, Expect: expect, AdvanceBaseline: o.AdvancesBaseline()}
		if o == nastraffic.OutcomeSample {
			w.Sample = &s
		}
		writes = append(writes, w)
	}
	if err := store.SavePoll(ctx, nasID, at, writes); err != nil {
		t.Fatalf("save poll: %v", err)
	}
}

var t0 = time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)

func TestFR_NET_006_TwoPollsMakeASampleAndCountersPast2To63Survive(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	seedNAS(ctx, t, pool, 1, "10.0.0.1", false)
	store := database.NAS()

	const big = uint64(1)<<63 + 1_000 // past BIGINT's range
	pollAt(ctx, t, store, 1, t0, iface("ether1", 1, big, 5))
	pollAt(ctx, t, store, 1, t0.Add(5*time.Minute), iface("ether1", 1, big+37_500_000, 5+75_000_000))

	prev, err := store.InterfaceBaselines(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if prev["ether1"].InOctets != big+37_500_000 {
		t.Errorf("the stored counter came back as %d — NUMERIC did not round-trip a value past 2^63", prev["ether1"].InOctets)
	}
	if n := countRows(ctx, t, pool, `SELECT count(*) FROM nas_interface_samples`); n != 1 {
		t.Fatalf("%d samples after two polls, want 1", n)
	}
	var in, out int64
	if err := pool.QueryRow(ctx, `SELECT in_bps, out_bps FROM nas_interface_samples`).Scan(&in, &out); err != nil {
		t.Fatal(err)
	}
	if in != 1_000_000 || out != 2_000_000 {
		t.Errorf("sample is %d/%d bit/s, want 1 and 2 Mbit/s", in, out)
	}
	// The success is recorded on the router.
	if n := countRows(ctx, t, pool, `SELECT count(*) FROM nas_devices WHERE id = 1 AND snmp_last_ok_at IS NOT NULL AND snmp_last_error IS NULL`); n != 1 {
		t.Error("a successful poll was not recorded on the router")
	}
}

// Two pollers read the same baseline and both try to save. Only the first
// lands; the second records neither a sample nor a baseline.
func TestFR_NET_006_TwoPollersCannotRecordTheSameIntervalTwice(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	seedNAS(ctx, t, pool, 1, "10.0.0.1", false)
	store := database.NAS()

	pollAt(ctx, t, store, 1, t0, iface("ether1", 1, 1_000, 1_000))
	prev, _ := store.InterfaceBaselines(ctx, 1)
	b := prev["ether1"]

	writeFor := func(at time.Time, in uint64) []nastraffic.Write {
		cur := iface("ether1", 1, in, 1_000)
		s, o := nastraffic.Rate(&b, cur, at)
		if o != nastraffic.OutcomeSample {
			t.Fatalf("outcome %v", o)
		}
		return []nastraffic.Write{{Iface: cur, Expect: &b, AdvanceBaseline: true, Sample: &s}}
	}
	first := t0.Add(5 * time.Minute)
	second := t0.Add(5*time.Minute + 2*time.Second)
	if err := store.SavePoll(ctx, 1, first, writeFor(first, 2_000)); err != nil {
		t.Fatal(err)
	}
	if err := store.SavePoll(ctx, 1, second, writeFor(second, 2_100)); err != nil {
		t.Fatal(err)
	}

	if n := countRows(ctx, t, pool, `SELECT count(*) FROM nas_interface_samples`); n != 1 {
		t.Errorf("%d samples, want 1 — the losing poller recorded the same interval again", n)
	}
	after, _ := store.InterfaceBaselines(ctx, 1)
	if after["ether1"].InOctets != 2_000 {
		t.Errorf("the baseline is %d — the losing poller overwrote the winner's", after["ether1"].InOctets)
	}
}

// The history key is the name: a router that renumbers ifIndex after a
// reboot keeps ether1's history on ether1.
func TestFR_NET_006_ARenumberedIfIndexKeepsItsHistory(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	seedNAS(ctx, t, pool, 1, "10.0.0.1", false)
	store := database.NAS()

	pollAt(ctx, t, store, 1, t0, iface("ether1", 1, 0, 0))
	pollAt(ctx, t, store, 1, t0.Add(5*time.Minute), iface("ether1", 7, 375_000, 375_000))

	if n := countRows(ctx, t, pool, `SELECT count(*) FROM nas_interfaces`); n != 1 {
		t.Fatalf("%d interface rows — a new ifIndex started a new history", n)
	}
	if n := countRows(ctx, t, pool, `SELECT count(*) FROM nas_interfaces WHERE if_index = 7`); n != 1 {
		t.Error("the new ifIndex was not recorded")
	}
	if n := countRows(ctx, t, pool, `SELECT count(*) FROM nas_interface_samples`); n != 1 {
		t.Error("the renumbered interface lost its sample")
	}
}

// A reading too soon after the last keeps the old baseline, so the next poll
// measures across both intervals instead of losing one.
func TestFR_NET_006_ATooSoonReadingKeepsTheBaseline(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	seedNAS(ctx, t, pool, 1, "10.0.0.1", false)
	store := database.NAS()

	pollAt(ctx, t, store, 1, t0, iface("ether1", 1, 100, 100))
	pollAt(ctx, t, store, 1, t0.Add(2*time.Second), iface("ether1", 1, 200, 200))
	prev, _ := store.InterfaceBaselines(ctx, 1)
	if prev["ether1"].InOctets != 100 || !prev["ether1"].At.Equal(t0) {
		t.Errorf("baseline moved to %+v on a too-soon reading", prev["ether1"])
	}
}

// Buckets average by interval and keep the peak; an unmeasured bucket is
// absent, never zero.
func TestFR_NET_006_TheSeriesIsTimeWeightedKeepsPeaksAndLeavesGaps(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	seedNAS(ctx, t, pool, 1, "10.0.0.1", false)
	store := database.NAS()
	pollAt(ctx, t, store, 1, t0, iface("ether1", 1, 0, 0))
	var id int
	if err := pool.QueryRow(ctx, `SELECT id FROM nas_interfaces`).Scan(&id); err != nil {
		t.Fatal(err)
	}

	// Bucket one (10:00–10:30): 300 s at 10 Mbit/s and 600 s at 40 Mbit/s.
	// The weighted mean is 30 Mbit/s; a plain mean of the two rates would
	// say 25. Bucket two (10:30–11:00) has no samples at all. Bucket three
	// has one.
	for _, s := range []struct {
		at   time.Time
		secs int
		bps  int64
	}{
		{t0.Add(5 * time.Minute), 300, 10_000_000},
		{t0.Add(15 * time.Minute), 600, 40_000_000},
		{t0.Add(65 * time.Minute), 300, 7_000_000},
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO nas_interface_samples VALUES ($1, $2, $3, $4, $4)`,
			id, s.at, s.secs, s.bps); err != nil {
			t.Fatal(err)
		}
	}

	pts, err := store.TrafficSeries(ctx, id, t0, t0.Add(90*time.Minute), 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 2 {
		t.Fatalf("%d buckets, want 2 — the empty half-hour must be absent, not zero: %+v", len(pts), pts)
	}
	if pts[0].InAvgBps != 30_000_000 {
		t.Errorf("weighted average %d, want 30000000 (a plain mean would be 25000000)", pts[0].InAvgBps)
	}
	if pts[0].InPeakBps != 40_000_000 {
		t.Errorf("peak %d, want 40000000", pts[0].InPeakBps)
	}
	if !pts[1].At.Equal(t0.Add(60 * time.Minute)) {
		t.Errorf("second bucket at %v, want 11:00", pts[1].At)
	}
}

func TestFR_NET_006_SettingsFailuresPurgeAndCascade(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	seedNAS(ctx, t, pool, 1, "10.0.0.1", false)
	seedNAS(ctx, t, pool, 2, "10.0.0.2", false)
	store := database.NAS()

	// Enabling with no community is refused with the domain error.
	if err := store.SetNASSNMP(ctx, 1, true, nil, 161); !errors.Is(err, nastraffic.ErrSNMPNeedsCommunity) {
		t.Errorf("enabling with no community gave %v", err)
	}
	enc := "v1:community"
	if err := store.SetNASSNMP(ctx, 1, true, &enc, 1161); err != nil {
		t.Fatal(err)
	}
	routers, err := store.ListPollableRouters(ctx)
	if err != nil || len(routers) != 1 || routers[0].NASID != 1 || routers[0].Port != 1161 ||
		routers[0].CommunityEncrypted != enc || routers[0].Host != "10.0.0.1" {
		t.Fatalf("pollable routers %+v, %v", routers, err)
	}
	// Re-saving without a community keeps the stored one.
	if err := store.SetNASSNMP(ctx, 1, true, nil, 1161); err != nil {
		t.Fatal(err)
	}

	// A failure records the reason and keeps the last success.
	pollAt(ctx, t, store, 1, t0, iface("ether1", 1, 0, 0))
	if err := store.RecordPollFailure(ctx, 1, t0.Add(5*time.Minute), "No answer"); err != nil {
		t.Fatal(err)
	}
	all, _ := store.ListTrafficRouters(ctx)
	r := all[0]
	if r.LastError != "No answer" || r.LastOKAt == nil || !r.LastOKAt.Equal(t0) || r.Interfaces != 1 || !r.HasCommunity {
		t.Errorf("router status %+v", r)
	}
	// Changing the settings clears the error, which described the old ones.
	if err := store.SetNASSNMP(ctx, 1, true, &enc, 161); err != nil {
		t.Fatal(err)
	}
	if all, _ = store.ListTrafficRouters(ctx); all[0].LastError != "" {
		t.Error("changing the settings left the old error showing")
	}

	// Purge removes only expired samples.
	var id int
	_ = pool.QueryRow(ctx, `SELECT id FROM nas_interfaces`).Scan(&id)
	_, _ = pool.Exec(ctx, `INSERT INTO nas_interface_samples VALUES ($1, $2, 300, 1, 1), ($1, $3, 300, 1, 1)`,
		id, t0.Add(-40*24*time.Hour), t0)
	if n, err := store.PurgeSamplesBefore(ctx, t0.Add(-nastraffic.Retention)); err != nil || n != 1 {
		t.Errorf("purged %d (%v), want 1", n, err)
	}

	// Removing the router takes its interfaces and samples with it.
	if _, err := pool.Exec(ctx, `DELETE FROM nas_devices WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if n := countRows(ctx, t, pool, `SELECT count(*) FROM nas_interfaces`); n != 0 {
		t.Error("a deleted router's interfaces survived")
	}
}
