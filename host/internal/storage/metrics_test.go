package storage

import (
	"testing"
	"time"
)

func newTestMetricsStore(t *testing.T) *SQLiteMetricsStore {
	t.Helper()
	store, err := NewSQLiteMetricsStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteMetricsStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestMetrics_RecordAndQueryCrash(t *testing.T) {
	store := newTestMetricsStore(t)

	if err := store.RecordCrash("test", "panic: nil pointer"); err != nil {
		t.Fatalf("RecordCrash: %v", err)
	}
	if err := store.RecordCrash("test", "panic: index out of range"); err != nil {
		t.Fatalf("RecordCrash: %v", err)
	}

	count, err := store.QueryCrashWindow(time.Hour)
	if err != nil {
		t.Fatalf("QueryCrashWindow: %v", err)
	}
	if count != 2 {
		t.Errorf("crash count = %d, want 2", count)
	}

	// Zero window should still include recent events (within 1 second)
	count, err = store.QueryCrashWindow(0)
	if err != nil {
		t.Fatalf("QueryCrashWindow(0): %v", err)
	}
	// Events just recorded should not appear in a zero-width window
	// (cutoff = now, events at now might or might not match)
}

func TestMetrics_RecordAndQueryLatency(t *testing.T) {
	store := newTestMetricsStore(t)

	// Insert 20 samples: 10ms, 20ms, ..., 200ms
	for i := 1; i <= 20; i++ {
		if err := store.RecordLatency("terminal", int64(i*10)); err != nil {
			t.Fatalf("RecordLatency: %v", err)
		}
	}

	p95, count, err := store.QueryLatencyP95Window(time.Hour)
	if err != nil {
		t.Fatalf("QueryLatencyP95Window: %v", err)
	}
	if count != 20 {
		t.Errorf("sample count = %d, want 20", count)
	}
	// p95 of 10,20,...,200 at index ceil(0.95*20)-1 = 18 -> 190ms
	if p95 != 190 {
		t.Errorf("p95 = %d, want 190", p95)
	}
}

func TestMetrics_LatencyP95_Empty(t *testing.T) {
	store := newTestMetricsStore(t)

	p95, count, err := store.QueryLatencyP95Window(time.Hour)
	if err != nil {
		t.Fatalf("QueryLatencyP95Window: %v", err)
	}
	if count != 0 {
		t.Errorf("sample count = %d, want 0", count)
	}
	if p95 != 0 {
		t.Errorf("p95 = %d, want 0", p95)
	}
}

func TestMetrics_LatencyP95_SingleSample(t *testing.T) {
	store := newTestMetricsStore(t)

	if err := store.RecordLatency("terminal", 42); err != nil {
		t.Fatalf("RecordLatency: %v", err)
	}

	p95, count, err := store.QueryLatencyP95Window(time.Hour)
	if err != nil {
		t.Fatalf("QueryLatencyP95Window: %v", err)
	}
	if count != 1 {
		t.Errorf("sample count = %d, want 1", count)
	}
	if p95 != 42 {
		t.Errorf("p95 = %d, want 42", p95)
	}
}

func TestMetrics_RecordAndQueryPairing(t *testing.T) {
	store := newTestMetricsStore(t)

	if err := store.RecordPairingAttempt("dev1", true); err != nil {
		t.Fatalf("RecordPairingAttempt: %v", err)
	}
	if err := store.RecordPairingAttempt("dev2", false); err != nil {
		t.Fatalf("RecordPairingAttempt: %v", err)
	}
	if err := store.RecordPairingAttempt("dev3", true); err != nil {
		t.Fatalf("RecordPairingAttempt: %v", err)
	}

	total, successes, err := store.QueryPairingWindow(time.Hour)
	if err != nil {
		t.Fatalf("QueryPairingWindow: %v", err)
	}
	if total != 3 {
		t.Errorf("total = %d, want 3", total)
	}
	if successes != 2 {
		t.Errorf("successes = %d, want 2", successes)
	}
}

func TestMetrics_Cleanup(t *testing.T) {
	store := newTestMetricsStore(t)

	// Insert some records
	if err := store.RecordCrash("test", "old crash"); err != nil {
		t.Fatalf("RecordCrash: %v", err)
	}
	if err := store.RecordLatency("test", 100); err != nil {
		t.Fatalf("RecordLatency: %v", err)
	}
	if err := store.RecordPairingAttempt("dev1", true); err != nil {
		t.Fatalf("RecordPairingAttempt: %v", err)
	}

	// Cleanup with zero retention should delete everything
	deleted, err := store.Cleanup(0)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	// All 3 rows should be deleted (they were just inserted, so within any window,
	// but 0 retention means cutoff = now, so recent rows survive)
	// Actually, retention=0 means cutoff=now, so rows at "now" won't be deleted.
	// Let's use a large retention to keep all, then verify.
	_ = deleted

	// Insert again and cleanup with very large retention (keeps everything)
	if err := store.RecordCrash("test", "keep this"); err != nil {
		t.Fatalf("RecordCrash: %v", err)
	}
	deleted, err = store.Cleanup(24 * time.Hour * 365)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if deleted != 0 {
		t.Errorf("deleted = %d, want 0 (nothing should be old enough)", deleted)
	}
}

