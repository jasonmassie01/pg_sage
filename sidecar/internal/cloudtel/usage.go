package cloudtel

import (
	"context"
	"math"
	"sort"
	"sync"
	"time"
)

// Hourly host usage (market-04): each good sample rolls into its
// resource's current hour, and every attached database's sink receives
// the hour's aggregate so far (it replaces the row). The aggregate is
// what right-sizing and cost attribution read later: host samples
// themselves live only in memory.

// usageWriteTimeout bounds one sink write, so a slow store never holds
// the poller.
const usageWriteTimeout = 5 * time.Second

// HourlyUsage is one resource-hour of host usage. Unknown values are nil
// (or 0 for sizes), never a guess.
type HourlyUsage struct {
	Resource              string
	Hour                  time.Time // UTC, truncated to the hour
	InstanceClass         string
	VCPUs                 int
	Serverless            bool
	MemoryTotalBytes      float64
	AllocatedStorageBytes float64
	CPUP50, CPUP95        *float64
	CPUMax                *float64
	MemAvailableMin       *float64
	DBLoadP95             *float64
	ConnectionsMax        *float64
	Samples               int
}

// UsageSink stores one hour's aggregate (sage.host_usage_hourly).
type UsageSink func(ctx context.Context, u HourlyUsage) error

// UsageErrorFunc is told of a failed write (the database and the row).
type UsageErrorFunc func(database string, u HourlyUsage, err error)

type usageSub struct {
	database string
	sink     UsageSink
	onErr    UsageErrorFunc
}

// usageState is the open hour and the attached sinks.
type usageState struct {
	mu     sync.Mutex
	subs   map[*usageSub]struct{}
	bucket *usageBucket
}

type usageBucket struct {
	resource, class string
	hour            time.Time
	vcpus           int
	serverless      bool
	memTotal, alloc float64
	cpu, load       []float64
	memMin, connMax *float64
	samples         int
}

// AttachUsage adds database's sink: each successful poll writes the
// current hour's aggregate to it. The detach function (idempotent) stops
// the writes. A nil sink attaches nothing.
func (r *Runtime) AttachUsage(database string, sink UsageSink, onErr UsageErrorFunc) func() {
	if sink == nil {
		return func() {}
	}
	sub := &usageSub{database: database, sink: sink, onErr: onErr}
	r.usage.mu.Lock()
	if r.usage.subs == nil {
		r.usage.subs = map[*usageSub]struct{}{}
	}
	r.usage.subs[sub] = struct{}{}
	r.usage.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.usage.mu.Lock()
			delete(r.usage.subs, sub)
			r.usage.mu.Unlock()
		})
	}
}

// rollUsage adds s to its hour and writes the aggregate to every sink.
// Callers hold pollMu, so buckets change one poll at a time.
func (r *Runtime) rollUsage(ctx context.Context, s Sample, now time.Time) {
	r.usage.mu.Lock()
	if len(r.usage.subs) == 0 {
		r.usage.mu.Unlock()
		return
	}
	b := r.usage.bucketFor(s, now)
	b.add(s, now)
	u := b.aggregate()
	subs := make([]*usageSub, 0, len(r.usage.subs))
	for sub := range r.usage.subs {
		subs = append(subs, sub)
	}
	r.usage.mu.Unlock()
	for _, sub := range subs {
		wctx, cancel := context.WithTimeout(ctx, usageWriteTimeout)
		err := sub.sink(wctx, u)
		cancel()
		if err != nil && sub.onErr != nil {
			sub.onErr(sub.database, u, err)
		}
	}
}

// bucketFor is the open bucket for s, starting a new one on a new hour,
// resource or size (a resize ends the old size's window).
func (st *usageState) bucketFor(s Sample, now time.Time) *usageBucket {
	at := s.CollectedAt
	if at.IsZero() {
		at = now
	}
	hour := at.UTC().Truncate(time.Hour)
	b := st.bucket
	if b == nil || !b.hour.Equal(hour) || b.resource != s.Resource ||
		b.class != s.InstanceClass || b.vcpus != s.VCPUs {
		b = &usageBucket{resource: s.Resource, class: s.InstanceClass, hour: hour,
			vcpus: s.VCPUs, serverless: s.Serverless}
		st.bucket = b
	}
	return b
}

// add counts the sample and keeps its fresh values.
func (b *usageBucket) add(s Sample, now time.Time) {
	b.samples++
	if cpu, err := s.CPU(now); err == nil {
		b.cpu = append(b.cpu, cpu)
	}
	if total, avail := s.HostMemory(now); total > 0 {
		b.memTotal = float64(total)
		if s.FreeableMemoryBytes.fresh(now) && s.FreeableMemoryBytes.Value <= float64(total) {
			b.memMin = minPtr(b.memMin, float64(avail))
		}
	}
	if s.AllocatedStorageBytes.fresh(now) {
		b.alloc = s.AllocatedStorageBytes.Value
	}
	if s.DBLoad.fresh(now) {
		b.load = append(b.load, s.DBLoad.Value)
	}
	if s.Connections.fresh(now) {
		b.connMax = maxPtr(b.connMax, s.Connections.Value)
	}
}

func (b *usageBucket) aggregate() HourlyUsage {
	u := HourlyUsage{Resource: b.resource, Hour: b.hour, InstanceClass: b.class,
		VCPUs: b.vcpus, Serverless: b.serverless, MemoryTotalBytes: b.memTotal,
		AllocatedStorageBytes: b.alloc, Samples: b.samples,
		CPUP50: NearestRank(b.cpu, 0.50), CPUP95: NearestRank(b.cpu, 0.95),
		CPUMax: NearestRank(b.cpu, 1), DBLoadP95: NearestRank(b.load, 0.95)}
	if b.memMin != nil {
		v := *b.memMin
		u.MemAvailableMin = &v
	}
	if b.connMax != nil {
		v := *b.connMax
		u.ConnectionsMax = &v
	}
	return u
}

// NearestRank is the nearest-rank percentile q (0-1] of values (nil when
// there are none).
func NearestRank(values []float64, q float64) *float64 {
	if len(values) == 0 || q <= 0 || q > 1 {
		return nil
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	i := int(math.Ceil(q*float64(len(sorted)))) - 1
	v := sorted[max(i, 0)]
	return &v
}

func minPtr(p *float64, v float64) *float64 {
	if p == nil || v < *p {
		return &v
	}
	return p
}

func maxPtr(p *float64, v float64) *float64 {
	if p == nil || v > *p {
		return &v
	}
	return p
}
