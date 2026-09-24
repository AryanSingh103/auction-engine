package loadgen

import (
	"testing"
	"time"
)

func TestPercentileNearestRank(t *testing.T) {
	// 1ms..100ms: the nearest-rank p-th percentile is exactly p*100 ms.
	var d []time.Duration
	for i := 100; i >= 1; i-- { // reversed: summarize must sort
		d = append(d, time.Duration(i)*time.Millisecond)
	}
	got := summarize(d)
	want := Latency{Count: 100, Mean: 50.5, P50: 50, P90: 90, P99: 99, P999: 100, Max: 100}
	if got != want {
		t.Errorf("summarize = %+v, want %+v", got, want)
	}
}

func TestPercentileEdgeCases(t *testing.T) {
	if got := summarize(nil); got != (Latency{}) {
		t.Errorf("summarize(nil) = %+v, want zero", got)
	}
	one := summarize([]time.Duration{7 * time.Millisecond})
	if one.P50 != 7 || one.P999 != 7 || one.Max != 7 {
		t.Errorf("single sample = %+v, want every percentile 7", one)
	}
	// p99 of 10 samples is the 10th (ceil(9.9)), i.e. the max: small
	// samples cannot support a tail percentile, and the report must not
	// pretend otherwise.
	var ten []time.Duration
	for i := 1; i <= 10; i++ {
		ten = append(ten, time.Duration(i)*time.Millisecond)
	}
	if p := summarize(ten).P99; p != 10 {
		t.Errorf("p99 of 10 samples = %v, want 10 (the max)", p)
	}
}
