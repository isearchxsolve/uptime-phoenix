package services

import (
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// minuteDown returns a one-minute DOWN interval starting at `offset` after `from`.
func minuteDown(from time.Time, offset time.Duration) domain.MonitorHealthInterval {
	start := from.Add(offset)
	return domain.MonitorHealthInterval{
		From:           start,
		To:             start.Add(time.Minute),
		Status:         domain.StatusDown,
		Reason:         "",
		Cause:          domain.HealthHistoryCauseRegional,
		Policy:         domain.HealthPolicyAllDown,
		PolicyRevision: 0,
	}
}

// SCENARIO_DEFECT_REGRESSION (regression): issue #198 — back-to-back 1-minute
// DOWN buckets of overall health history must count as a single outage, not one
// per bucket. A continuous 24h outage must report exactly 1 outage.
func TestApplyOverallProjection_ConsecutiveDownBucketsAreOneOutage(t *testing.T) {
	from := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	intervals := make([]domain.MonitorHealthInterval, 0, 24*60)
	for m := 0; m < 24*60; m++ {
		intervals = append(intervals, minuteDown(from, time.Duration(m)*time.Minute))
	}
	got := applyOverallProjection(InsightsRow{}, intervals, from, to)
	if got.OutageCount != 1 {
		t.Fatalf("outage count = %d, want 1 for a continuous 24h down outage", got.OutageCount)
	}
	if got.DowntimeSeconds != int64(24*time.Hour/time.Second) {
		t.Fatalf("downtime = %d, want 86400", got.DowntimeSeconds)
	}
}

// SCENARIO_HAPPY_PATH (happy_path): a single down entry inside the window counts
// as exactly one outage with the matching down/up flaps.
func TestApplyOverallProjection_EnteringDownWithinWindowCountsOnce(t *testing.T) {
	from := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	intervals := []domain.MonitorHealthInterval{
		{From: from, To: from.Add(6 * time.Hour), Status: domain.StatusUp},
		{From: from.Add(6 * time.Hour), To: from.Add(7 * time.Hour), Status: domain.StatusDown},
		{From: from.Add(7 * time.Hour), To: to, Status: domain.StatusUp},
	}
	got := applyOverallProjection(InsightsRow{}, intervals, from, to)
	if got.OutageCount != 1 {
		t.Fatalf("outage count = %d, want 1 for a single down entry", got.OutageCount)
	}
	if got.FlapCount != 2 {
		t.Fatalf("flap count = %d, want 2 (down then up)", got.FlapCount)
	}
}

// Two separated down runs must count as two outages; adjacency is what merges
// buckets, not mere down status.
func TestApplyOverallProjection_SeparateDownRunsAreSeparateOutages(t *testing.T) {
	from := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	intervals := []domain.MonitorHealthInterval{
		{From: from, To: from.Add(time.Hour), Status: domain.StatusUp},
		{From: from.Add(time.Hour), To: from.Add(2 * time.Hour), Status: domain.StatusDown}, // outage 1
		{From: from.Add(2 * time.Hour), To: from.Add(3 * time.Hour), Status: domain.StatusUp},
		{From: from.Add(3 * time.Hour), To: from.Add(4 * time.Hour), Status: domain.StatusDown}, // outage 2
		{From: from.Add(4 * time.Hour), To: to, Status: domain.StatusUp},
	}
	got := applyOverallProjection(InsightsRow{}, intervals, from, to)
	if got.OutageCount != 2 {
		t.Fatalf("outage count = %d, want 2 for two separated down runs", got.OutageCount)
	}
}

// SCENARIO_NEGATIVE_FAILURE (negative): an outage that began before the window
// is an open outage, not a new one — it must not be counted.
func TestApplyOverallProjection_OpenOutageStartedBeforeWindow(t *testing.T) {
	from := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	got := applyOverallProjection(InsightsRow{}, []domain.MonitorHealthInterval{
		{From: from.Add(-time.Hour), To: to, Status: domain.StatusDown},
	}, from, to)
	if got.OutageCount != 0 {
		t.Fatalf("outage count = %d, want 0 for an outage that started before the window", got.OutageCount)
	}
}

// An all-up monitor reports zero outages.
func TestApplyOverallProjection_AllUpHasNoOutages(t *testing.T) {
	from := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	got := applyOverallProjection(InsightsRow{}, []domain.MonitorHealthInterval{
		{From: from, To: to, Status: domain.StatusUp},
	}, from, to)
	if got.OutageCount != 0 {
		t.Fatalf("outage count = %d, want 0 for an all-up monitor", got.OutageCount)
	}
}

// SCENARIO_NULL_INPUT (null): nil interval list is handled gracefully.
func TestApplyOverallProjection_NilIntervals(t *testing.T) {
	from := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	got := applyOverallProjection(InsightsRow{}, nil, from, to)
	if got.OutageCount != 0 {
		t.Fatalf("outage count = %d, want 0 for nil intervals", got.OutageCount)
	}
}

// SCENARIO_EMPTY_INPUT (empty): an empty interval list is handled gracefully.
func TestApplyOverallProjection_EmptyIntervals(t *testing.T) {
	from := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	got := applyOverallProjection(InsightsRow{}, []domain.MonitorHealthInterval{}, from, to)
	if got.OutageCount != 0 {
		t.Fatalf("outage count = %d, want 0 for empty intervals", got.OutageCount)
	}
}

// SCENARIO_BOUNDARY_LIMIT (boundary): down entering exactly at `from` counts;
// down starting exactly at `to` does not; an inverted range yields nothing.
func TestApplyOverallProjection_OutageWindowBoundaries(t *testing.T) {
	from := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	intervals := []domain.MonitorHealthInterval{
		{From: from.Add(-2 * time.Hour), To: from, Status: domain.StatusUp}, // up until `from`
		{From: from, To: from.Add(time.Hour), Status: domain.StatusDown},    // enters down exactly at `from` -> counted
		{From: from.Add(time.Hour), To: from.Add(2 * time.Hour), Status: domain.StatusUp},
		{From: from.Add(2 * time.Hour), To: from.Add(3 * time.Hour), Status: domain.StatusDown}, // enters down within window -> counted
		{From: to, To: to.Add(time.Hour), Status: domain.StatusDown},                            // starts exactly at `to` -> not counted
	}
	got := applyOverallProjection(InsightsRow{}, intervals, from, to)
	if got.OutageCount != 2 {
		t.Fatalf("outage count = %d, want 2 (boundary at `from` counts, at `to` does not)", got.OutageCount)
	}
}

func TestApplyOverallProjection_InvertedTimeRange(t *testing.T) {
	from := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	to := from.Add(-time.Hour) // inverted: to before from
	intervals := []domain.MonitorHealthInterval{
		{From: from, To: from.Add(time.Hour), Status: domain.StatusDown},
	}
	got := applyOverallProjection(InsightsRow{}, intervals, from, to)
	if got.OutageCount != 0 {
		t.Fatalf("outage count = %d, want 0 for an inverted time range", got.OutageCount)
	}
}

// SCENARIO_CONCURRENCY_RACE (concurrency): concurrent calls on the same intervals
// must produce identical outage counts (applyOverallProjection is stateless).
func TestApplyOverallProjection_ConcurrentCallsAreConsistent(t *testing.T) {
	from := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	intervals := make([]domain.MonitorHealthInterval, 0, 60)
	for m := 0; m < 60; m++ {
		start := from.Add(time.Duration(m) * time.Minute)
		status := domain.StatusUp
		if m%2 == 0 {
			status = domain.StatusDown
		}
		intervals = append(intervals, domain.MonitorHealthInterval{
			From: start, To: start.Add(time.Minute), Status: status,
		})
	}
	const workers = 10
	results := make([]int, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got := applyOverallProjection(InsightsRow{}, intervals, from, to)
			results[i] = got.OutageCount
		}(i)
	}
	wg.Wait()
	for i, n := range results {
		if n != 30 {
			t.Fatalf("worker %d: outage count = %d, want 30", i, n)
		}
	}
}
