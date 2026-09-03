package fup

import (
	"context"
	"testing"
	"time"
)

// countingAlerter records every Trigger so a test can assert how many pages
// a sequence of polls would actually have produced.
type countingAlerter struct {
	events []int
}

func (a *countingAlerter) Trigger(_ string, detail any) {
	n, _ := detail.(int)
	a.events = append(a.events, n)
}

// fixedCounter returns a scripted sequence of dead-letter depths, one per
// poll, holding the final value once exhausted.
type fixedCounter struct {
	seq []int
	i   int
}

func (c *fixedCounter) DeadCount(context.Context, string) (int, error) {
	v := c.seq[min(c.i, len(c.seq)-1)]
	c.i++
	return v, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// newTestMonitor builds a monitor with a controllable clock so reminder
// behaviour is testable without sleeping.
func newTestMonitor(counter DeadCounter, alerter Alerter, clock func() time.Time) *DeadLetterMonitor {
	m := NewDeadLetterMonitor(counter, alerter)
	m.now = clock
	return m
}

// TestDeadLetterMonitor_DoesNotRepeatOnUnchangedQueue is the regression test
// for the alert storm this monitor actually produced: 2,400 identical pages
// from two stuck tasks over roughly a week, because the old checkOnce fired
// on every poll while the count was above zero.
//
// The assertion is deliberately about the *count* of alerts across many
// polls, because that is the thing that was wrong. The old code passed every
// test that only asked "does it alert when the queue is non-empty" — it did,
// 2,400 times.
func TestDeadLetterMonitor_DoesNotRepeatOnUnchangedQueue(t *testing.T) {
	alerter := &countingAlerter{}
	clock := time.Now()
	m := newTestMonitor(&fixedCounter{seq: []int{2}}, alerter, func() time.Time { return clock })

	// 100 polls with the queue stuck at the same depth — about 50 minutes at
	// the default 30s interval.
	for i := 0; i < 100; i++ {
		if err := m.checkOnce(context.Background()); err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}
	}

	if len(alerter.events) != 1 {
		t.Errorf("100 polls of an unchanged queue produced %d alerts, want exactly 1.\n"+
			"This is the storm that made the real alert unreadable: repeating a page that "+
			"says nothing new trains people to filter it, and the filter catches the next "+
			"genuine incident too.", len(alerter.events))
	}
}

// TestDeadLetterMonitor_AlertsWhenTheQueueGrows — a queue that keeps
// acquiring casualties is a different situation from one sitting on the same
// two failures, and must page again.
func TestDeadLetterMonitor_AlertsWhenTheQueueGrows(t *testing.T) {
	alerter := &countingAlerter{}
	clock := time.Now()
	// Stable, then worse, then stable again at the higher level.
	m := newTestMonitor(&fixedCounter{seq: []int{2, 2, 2, 5, 5, 5}}, alerter, func() time.Time { return clock })

	for i := 0; i < 6; i++ {
		_ = m.checkOnce(context.Background())
	}

	if len(alerter.events) != 2 {
		t.Fatalf("want 2 alerts (first sighting, then growth), got %d: %v", len(alerter.events), alerter.events)
	}
	if alerter.events[0] != 2 || alerter.events[1] != 5 {
		t.Errorf("alert payloads: got %v, want [2 5]", alerter.events)
	}
}

// TestDeadLetterMonitor_RemindsAboutAStillBrokenQueue — silence must not be
// permanent. A queue stuck for hours should resurface, just far less often
// than every poll.
func TestDeadLetterMonitor_RemindsAboutAStillBrokenQueue(t *testing.T) {
	alerter := &countingAlerter{}
	clock := time.Now()
	m := newTestMonitor(&fixedCounter{seq: []int{3}}, alerter, func() time.Time { return clock })
	m.SetReminderInterval(time.Hour)

	_ = m.checkOnce(context.Background()) // first sighting
	if len(alerter.events) != 1 {
		t.Fatalf("want 1 alert after first sighting, got %d", len(alerter.events))
	}

	// Half an hour later: still broken, still quiet.
	clock = clock.Add(30 * time.Minute)
	_ = m.checkOnce(context.Background())
	if len(alerter.events) != 1 {
		t.Errorf("re-alerted after 30m with a 1h reminder interval: %d alerts", len(alerter.events))
	}

	// Past the reminder: resurface.
	clock = clock.Add(31 * time.Minute)
	_ = m.checkOnce(context.Background())
	if len(alerter.events) != 2 {
		t.Errorf("want a reminder alert after the interval elapsed, got %d alerts", len(alerter.events))
	}
}

// TestDeadLetterMonitor_RecoveryResetsState — after the queue drains, a
// later recurrence is a new incident and must page immediately rather than
// waiting out a reminder interval inherited from the previous one.
func TestDeadLetterMonitor_RecoveryResetsState(t *testing.T) {
	alerter := &countingAlerter{}
	clock := time.Now()
	m := newTestMonitor(&fixedCounter{seq: []int{4, 4, 0, 0, 4}}, alerter, func() time.Time { return clock })

	for i := 0; i < 5; i++ {
		_ = m.checkOnce(context.Background())
	}

	if len(alerter.events) != 2 {
		t.Errorf("want 2 alerts (initial incident, then a fresh one after recovery), got %d: %v",
			len(alerter.events), alerter.events)
	}
}

// TestDeadLetterMonitor_SilentWhileHealthy — an empty queue must never page,
// however many times it is polled.
func TestDeadLetterMonitor_SilentWhileHealthy(t *testing.T) {
	alerter := &countingAlerter{}
	clock := time.Now()
	m := newTestMonitor(&fixedCounter{seq: []int{0}}, alerter, func() time.Time { return clock })

	for i := 0; i < 20; i++ {
		_ = m.checkOnce(context.Background())
	}
	if len(alerter.events) != 0 {
		t.Errorf("an empty queue produced %d alerts", len(alerter.events))
	}
}
