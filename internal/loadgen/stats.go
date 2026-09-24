package loadgen

import (
	"math"
	"slices"
	"time"
)

// Latency summarizes a set of request durations, in milliseconds.
type Latency struct {
	Count int     `json:"count"`
	Mean  float64 `json:"mean_ms"`
	P50   float64 `json:"p50_ms"`
	P90   float64 `json:"p90_ms"`
	P99   float64 `json:"p99_ms"`
	P999  float64 `json:"p99_9_ms"`
	Max   float64 `json:"max_ms"`
}

// summarize computes exact percentiles from every recorded duration (not a
// histogram estimate). It sorts its input in place.
func summarize(d []time.Duration) Latency {
	if len(d) == 0 {
		return Latency{}
	}
	slices.Sort(d)
	var sum time.Duration
	for _, v := range d {
		sum += v
	}
	ms := func(v time.Duration) float64 { return float64(v.Microseconds()) / 1000 }
	return Latency{
		Count: len(d),
		Mean:  ms(sum / time.Duration(len(d))),
		P50:   ms(percentile(d, 0.50)),
		P90:   ms(percentile(d, 0.90)),
		P99:   ms(percentile(d, 0.99)),
		P999:  ms(percentile(d, 0.999)),
		Max:   ms(d[len(d)-1]),
	}
}

// percentile returns the nearest-rank percentile of sorted: the smallest
// value such that at least p of the samples are less than or equal to it.
func percentile(sorted []time.Duration, p float64) time.Duration {
	rank := int(math.Ceil(p * float64(len(sorted))))
	return sorted[max(rank, 1)-1]
}
