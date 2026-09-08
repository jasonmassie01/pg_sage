package providerobs

import (
	"bufio"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type MetricSnapshot struct {
	ObservedAt           time.Time
	IdleCPUSeconds       map[string]float64
	MemoryTotalBytes     *float64
	MemoryAvailableBytes *float64
}

// Only documented node exporter metrics are interpreted. Unknown series remain untouched.
var metricLine = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{[^}]*\})?\s+(\S+)(?:\s+\S+)?$`)
var labelPair = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)="((?:[^"\\]|\\.)*)"`)

func ParseMetrics(body string, now time.Time) (MetricSnapshot, error) {
	s := MetricSnapshot{IdleCPUSeconds: make(map[string]float64)}
	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := metricLine.FindStringSubmatch(line)
		if parts == nil {
			return s, errors.New("invalid Prometheus exposition line")
		}
		if !wantedMetric(parts[1]) {
			continue
		}
		value, err := strconv.ParseFloat(parts[3], 64)
		if err != nil || !validNumber(value) {
			return s, errors.New("invalid provider metric value")
		}
		if err := s.add(parts[1], parts[2], value); err != nil {
			return s, err
		}
	}
	if err := scanner.Err(); err != nil {
		return s, errors.New("provider metric line exceeds limit")
	}
	if !validAge(s.ObservedAt, now) {
		return s, errors.New("provider timestamp missing, stale or future")
	}
	if s.MemoryTotalBytes != nil && s.MemoryAvailableBytes != nil &&
		*s.MemoryAvailableBytes > *s.MemoryTotalBytes {
		return s, errors.New("available memory exceeds total memory")
	}
	return s, nil
}

func wantedMetric(name string) bool {
	return name == "node_time_seconds" || name == "node_cpu_seconds_total" ||
		name == "node_memory_MemTotal_bytes" || name == "node_memory_MemAvailable_bytes"
}

func (s *MetricSnapshot) add(name, labels string, value float64) error {
	switch name {
	case "node_time_seconds":
		if !s.ObservedAt.IsZero() {
			return errors.New("multiple provider hosts require explicit isolation")
		}
		s.ObservedAt = time.Unix(int64(value), int64((value-float64(int64(value)))*1e9)).UTC()
	case "node_cpu_seconds_total":
		parsed := make(map[string]string)
		for _, pair := range labelPair.FindAllStringSubmatch(labels, -1) {
			parsed[pair[1]] = pair[2]
		}
		if parsed["mode"] != "idle" {
			return nil
		}
		if parsed["cpu"] == "" {
			return errors.New("CPU metric lacks core identity")
		}
		if _, ok := s.IdleCPUSeconds[labels]; ok {
			return errors.New("duplicate CPU metric")
		}
		s.IdleCPUSeconds[labels] = value
	case "node_memory_MemTotal_bytes":
		if s.MemoryTotalBytes != nil || value == 0 {
			return errors.New("invalid total memory metric")
		}
		s.MemoryTotalBytes = &value
	case "node_memory_MemAvailable_bytes":
		if s.MemoryAvailableBytes != nil {
			return errors.New("duplicate available memory metric")
		}
		s.MemoryAvailableBytes = &value
	}
	return nil
}
