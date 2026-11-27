package sre

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// The temp-file window peak (footprint-04): self-configuration samples
// the largest temp-file growth the detector measured within one window
// since the previous read, instead of an hourly average that never
// crosses the threshold it judges.

const mib = 1 << 20

func TestDetector_TempPeakIsTheLargestWindowSinceTheLastTake(t *testing.T) {
	base := float64(5 << 30)
	r := newSeqRunner().add(tempPoll(0, base), tempPoll(time.Minute, base+100*mib),
		tempPoll(2*time.Minute, base+700*mib), tempPoll(10*time.Minute, base+750*mib),
		tempPoll(11*time.Minute, base+760*mib))
	d, _ := newDetector(t, r)
	if v, ok := d.TakeTempPeak(); ok || v != 0 {
		t.Fatalf("a detector that never polled reported a peak %v", v)
	}
	poll(t, d)
	if v, ok := d.TakeTempPeak(); !ok || v != 0 {
		t.Fatalf("one poll: peak %v (%v), want 0 measured", v, ok)
	}
	poll(t, d)
	poll(t, d)
	if v, ok := d.TakeTempPeak(); !ok || v != 700*mib {
		t.Fatalf("peak %v MiB (%v), want 700", v/mib, ok)
	}
	if v, ok := d.TakeTempPeak(); ok || v != 0 {
		t.Fatalf("a second take without a poll reported %v", v/mib)
	}
	// The 10-minute poll is alone in its 5-minute window (0 MiB); the
	// 11-minute one measures 10 MiB since it. The old 700 MiB is gone.
	poll(t, d)
	poll(t, d)
	v, ok := d.TakeTempPeak()
	if !ok || v != 10*mib {
		t.Fatalf("peak after the old growth left the window: %v MiB (%v), want 10", v/mib, ok)
	}
}

func TestDetector_TempPeakIgnoresACounterReset(t *testing.T) {
	r := newSeqRunner().add(tempPoll(0, 9<<30), tempPoll(time.Minute, 1<<20),
		tempPoll(2*time.Minute, 3<<20))
	d, _ := newDetector(t, r)
	poll(t, d)
	poll(t, d)
	poll(t, d)
	if v, ok := d.TakeTempPeak(); !ok || v != 2<<20 {
		t.Fatalf("peak across a reset %v MiB (%v), want 2", v/mib, ok)
	}
}

func TestDetector_TempPeakUnknownWhenTheProbeFails(t *testing.T) {
	r := newSeqRunner().add(probes.Result{ProbeID: probes.TempFileActivity,
		Status: probes.StatusError, Reason: "permission_denied", ObservedAt: dt0})
	d, _ := newDetector(t, r)
	poll(t, d)
	poll(t, d)
	if v, ok := d.TakeTempPeak(); ok || v != 0 {
		t.Fatalf("a failed probe reported a peak %v", v)
	}
}

// Taking the peak never waits for a poll in progress and loses no growth.
func TestDetector_TempPeakConcurrentWithPolls(t *testing.T) {
	var polls []probes.Result
	for i := 0; i < 50; i++ {
		polls = append(polls, tempPoll(time.Duration(i)*time.Minute, float64(i)*mib))
	}
	d, _ := newDetector(t, newSeqRunner().add(polls...))
	var wg sync.WaitGroup
	var mu sync.Mutex
	var largest float64
	for g := 0; g < 4; g++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < 12; i++ {
				if _, err := d.Triggers(context.Background()); err != nil {
					t.Errorf("Triggers: %v", err)
				}
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if v, ok := d.TakeTempPeak(); ok {
					mu.Lock()
					largest = max(largest, v)
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	if v, ok := d.TakeTempPeak(); ok {
		largest = max(largest, v)
	}
	// Polls 0..47 ran; the last full 5-minute window grew 5 MiB.
	if largest != 5*mib {
		t.Fatalf("largest peak %v MiB, want 5", largest/mib)
	}
}
