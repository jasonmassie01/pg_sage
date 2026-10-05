package cloudtel

import (
	"strconv"
	"strings"
)

// RDS/Aurora instance class memory, from the AWS instance-class tables:
// burstable classes by size; general purpose (m), memory optimized (r) and
// x2g families scale linearly from their "large" size. An unknown class
// is unknown (0), never a guess.

const gibF = float64(1 << 30)

var burstableGiB = map[string]float64{"micro": 1, "small": 2, "medium": 4, "large": 8,
	"xlarge": 16, "2xlarge": 32}

// largeGiB is the memory of a family's "large" size (2 vCPUs).
var largeGiB = map[string]float64{
	"m5": 8, "m5d": 8, "m6g": 8, "m6gd": 8, "m6i": 8, "m6id": 8, "m7g": 8, "m7gd": 8,
	"m7i": 8, "m8g": 8,
	"r5": 16, "r5b": 16, "r5d": 16, "r6g": 16, "r6gd": 16, "r6i": 16, "r6id": 16,
	"r7g": 16, "r7gd": 16, "r7i": 16, "r8g": 16,
	"x2g": 32,
}

// InstanceClassMemoryBytes is the documented RAM of class (0: unknown).
func InstanceClassMemoryBytes(class string) float64 {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(class)), ".")
	if len(parts) != 3 || parts[0] != "db" {
		return 0
	}
	family, size := parts[1], parts[2]
	if family == "t3" || family == "t4g" {
		return burstableGiB[size] * gibF
	}
	base, ok := largeGiB[family]
	if !ok {
		return 0
	}
	mult := sizeMultiplier(size)
	return base * mult * gibF
}

// sizeMultiplier: large = 1, xlarge = 2, Nxlarge = 2N (0: unknown size).
func sizeMultiplier(size string) float64 {
	switch size {
	case "large":
		return 1
	case "xlarge":
		return 2
	}
	n, ok := strings.CutSuffix(size, "xlarge")
	if !ok {
		return 0
	}
	v, err := strconv.Atoi(n)
	if err != nil || v < 2 || v > 64 {
		return 0
	}
	return float64(2 * v)
}
