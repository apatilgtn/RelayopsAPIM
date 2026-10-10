package analytics

import (
	"context"
	"fmt"
	"math"
	"testing"

	"github.com/relayops/apim/internal/realtime"
	"github.com/relayops/apim/internal/store"
)

type captureWriter struct{ logs []store.RequestLog }

func (c *captureWriter) InsertLogs(_ context.Context, logs []store.RequestLog) error {
	c.logs = append(c.logs, logs...)
	return nil
}

func sampleBatch(successes, errors int) []store.RequestLog {
	var b []store.RequestLog
	for i := 0; i < successes; i++ {
		b = append(b, store.RequestLog{LogID: fmt.Sprintf("ok-%d", i), Status: 200})
	}
	for i := 0; i < errors; i++ {
		b = append(b, store.RequestLog{LogID: fmt.Sprintf("err-%d", i), Status: 502})
	}
	return b
}

func TestSamplingKeepsErrorsAndWeightsSuccesses(t *testing.T) {
	w := &captureWriter{}
	c := NewCollectorWithWriter(w, realtime.NewHub(), "n1")
	c.SetSampleRate(0.1)
	c.flush(context.Background(), sampleBatch(10000, 37))

	var errors, successes int
	var weighted float64
	for _, l := range w.logs {
		weight := l.SampleWeight
		if weight == 0 {
			weight = 1
		}
		weighted += weight
		if l.Status >= 400 {
			errors++
			if l.SampleWeight != 0 {
				t.Fatalf("error log carries weight %v; errors must be kept at weight 1", l.SampleWeight)
			}
		} else {
			successes++
		}
	}
	if errors != 37 {
		t.Fatalf("kept %d of 37 errors; every error must be persisted", errors)
	}
	if successes < 800 || successes > 1200 {
		t.Fatalf("kept %d of 10000 successes at rate 0.1", successes)
	}
	// The weighted total estimates the real request count.
	if math.Abs(weighted-10037)/10037 > 0.1 {
		t.Fatalf("weighted total %.0f, want about 10037", weighted)
	}
	if got := c.sampledOut.Load(); got != int64(10000-successes) {
		t.Fatalf("sampled-out counter %d, want %d", got, 10000-successes)
	}
}

func TestSamplingIsDeterministicAndOffByDefault(t *testing.T) {
	w := &captureWriter{}
	c := NewCollectorWithWriter(w, realtime.NewHub(), "n1")
	c.flush(context.Background(), sampleBatch(500, 5))
	if len(w.logs) != 505 {
		t.Fatalf("default rate persisted %d of 505", len(w.logs))
	}

	a, b := &captureWriter{}, &captureWriter{}
	ca := NewCollectorWithWriter(a, realtime.NewHub(), "n1")
	cb := NewCollectorWithWriter(b, realtime.NewHub(), "n2")
	ca.SetSampleRate(0.25)
	cb.SetSampleRate(0.25)
	ca.flush(context.Background(), sampleBatch(400, 0))
	cb.flush(context.Background(), sampleBatch(400, 0))
	if len(a.logs) != len(b.logs) {
		t.Fatalf("same log IDs sampled differently: %d vs %d", len(a.logs), len(b.logs))
	}
}
