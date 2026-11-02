package services

import (
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// TestApplyOverallRegression_ContiguousDownBucketsCountAsOneOutage is the
// regression test for issue #198: every 1-minute DOWN bucket in
// monitor_health_history was being counted as its own outage, inflating counts
// (e.g. 459 outages for one continuous 7-hour outage). Contiguous same-status
// intervals must count as a single outage — only the transition INTO Down counts.
func TestApplyOverallRegression_ContiguousDownBucketsCountAsOneOutage(t *testing.T) {
	from := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)

	intervals := make([]domain.MonitorHealthInterval, 0, 420)
	for i := 0; i < 420; i++ {
		start := from.Add(time.Duration(i) * time.Minute)
		intervals = append(intervals, domain.MonitorHealthInterval{
			From:   start,
			To:     start.Add(time.Minute),
			Status: domain.StatusDown,
		})
	}

	got := applyOverallProjection(InsightsRow{}, intervals, from, to)
	if got.OutageCount != 1 {
		t.Fatalf("contiguous DOWN buckets: outage_count = %d, want 1", got.OutageCount)
	}
	if got.DowntimeSeconds != int64(7*time.Hour/time.Second) {
		t.Fatalf("downtime_seconds = %d, want 25200 (7h)", got.DowntimeSeconds)
	}
}

// TestApplyOverallProjection_HappyPath_SingleDownTransition counts a single
// transition into DOWN as exactly one outage.
func TestApplyOverallProjection_HappyPath_SingleDownTransition(t *testing.T) {
	from := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)

	intervals := []domain.MonitorHealthInterval{
		{From: from, To: from.Add(12 * time.Hour), Status: domain.StatusUp},
		{From: from.Add(12 * time.Hour), To: from.Add(13 * time.Hour), Status: domain.StatusDown},
		{From: from.Add(13 * time.Hour), To: to, Status: domain.StatusUp},
	}

	got := applyOverallProjection(InsightsRow{}, intervals, from, to)
	if got.OutageCount != 1 {
		t.Fatalf("single DOWN transition: outage_count = %d, want 1", got.OutageCount)
	}
	if got.FlapCount != 2 {
		t.Fatalf("flap_count = %d, want 2 (UP->DOWN and DOWN->UP)", got.FlapCount)
	}
}

// TestApplyOverallProjection_NullOrEmptyIntervals returns zero outages for no
// history data, without panicking.
func TestApplyOverallProjection_NullOrEmptyIntervals(t *testing.T) {
	from := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)

	got := applyOverallProjection(InsightsRow{}, nil, from, to)
	if got.OutageCount != 0 {
		t.Fatalf("nil intervals: outage_count = %d, want 0", got.OutageCount)
	}

	got = applyOverallProjection(InsightsRow{}, []domain.MonitorHealthInterval{}, from, to)
	if got.OutageCount != 0 {
		t.Fatalf("empty intervals: outage_count = %d, want 0", got.OutageCount)
	}
}

// TestApplyOverallProjection_Boundary_DownAtWindowEdges checks outage counting
// at exact window boundaries.
func TestApplyOverallProjection_Boundary_DownAtWindowEdges(t *testing.T) {
	from := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)

	t.Run("down starts exactly at from", func(t *testing.T) {
		intervals := []domain.MonitorHealthInterval{
			{From: from, To: to, Status: domain.StatusDown},
		}
		got := applyOverallProjection(InsightsRow{}, intervals, from, to)
		if got.OutageCount != 1 {
			t.Fatalf("down starts at from: outage_count = %d, want 1", got.OutageCount)
		}
	})

	t.Run("down starts before from and continues", func(t *testing.T) {
		intervals := []domain.MonitorHealthInterval{
			{From: from.Add(-time.Hour), To: to, Status: domain.StatusDown},
		}
		got := applyOverallProjection(InsightsRow{}, intervals, from, to)
		if got.OutageCount != 0 {
			t.Fatalf("down started before window: outage_count = %d, want 0", got.OutageCount)
		}
	})

	t.Run("down spans full window with up before", func(t *testing.T) {
		intervals := []domain.MonitorHealthInterval{
			{From: from.Add(-time.Hour), To: from, Status: domain.StatusUp},
			{From: from, To: to, Status: domain.StatusDown},
		}
		got := applyOverallProjection(InsightsRow{}, intervals, from, to)
		if got.OutageCount != 1 {
			t.Fatalf("up then down at from: outage_count = %d, want 1", got.OutageCount)
		}
	})

	t.Run("down then up then down within window", func(t *testing.T) {
		intervals := []domain.MonitorHealthInterval{
			{From: from, To: from.Add(6 * time.Hour), Status: domain.StatusDown},
			{From: from.Add(6 * time.Hour), To: from.Add(12 * time.Hour), Status: domain.StatusUp},
			{From: from.Add(12 * time.Hour), To: to, Status: domain.StatusDown},
		}
		got := applyOverallProjection(InsightsRow{}, intervals, from, to)
		if got.OutageCount != 2 {
			t.Fatalf("two separate outages: outage_count = %d, want 2", got.OutageCount)
		}
	})
}

// TestApplyOverallProjection_Negative_FromAfterTo rejects an invalid window
// where from is not before to.
func TestApplyOverallProjection_Negative_FromAfterTo(t *testing.T) {
	from := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	to := from.Add(-time.Hour)

	intervals := []domain.MonitorHealthInterval{
		{From: from, To: to.Add(24 * time.Hour), Status: domain.StatusDown},
	}
	got := applyOverallProjection(InsightsRow{}, intervals, from, to)
	if got.OutageCount != 0 {
		t.Fatalf("invalid window (from >= to): outage_count = %d, want 0", got.OutageCount)
	}
}

// TestApplyOverallProjection_ConcurrentConcurrentCallsSafely verifies that
// concurrent calls to applyOverallProjection with the same input data produce
// consistent results (it is a pure function with no shared mutable state).
func TestApplyOverallProjection_ConcurrentConcurrentCallsSafely(t *testing.T) {
	from := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)

	intervals := []domain.MonitorHealthInterval{
		{From: from, To: from.Add(12 * time.Hour), Status: domain.StatusUp},
		{From: from.Add(12 * time.Hour), To: to, Status: domain.StatusDown},
	}

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			got := applyOverallProjection(InsightsRow{}, intervals, from, to)
			if got.OutageCount != 1 {
				t.Errorf("concurrent call: outage_count = %d, want 1", got.OutageCount)
			}
		}()
	}
	wg.Wait()
}
