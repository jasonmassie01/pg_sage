package cloudtel

import (
	"regexp"
	"strconv"
	"strings"
)

// Azure Database for PostgreSQL flexible server memory by SKU, from
// Microsoft's "Compute options" table (dated 2026-07-08, read 2026-11-12):
// Burstable sizes are listed; General Purpose (D) is 4 GiB per vCore in
// every series; Memory Optimized (E) is 8 GiB per vCore up to 48 vCores,
// with the larger sizes listed. An unknown SKU is unknown (0), never a guess.

var azureBurstableGiB = map[string]float64{"b1ms": 2, "b2s": 4, "b2ms": 8, "b4ms": 16,
	"b8ms": 32, "b12ms": 48, "b16ms": 64, "b20ms": 80}

var azureSKUPattern = regexp.MustCompile(
	`^standard_([de])(\d+)(s_v3|ds_v4|ds_v5|ads_v5|ds_v6|ads_v6)$`)

// azureVCores lists the vCore counts each series offers.
var azureVCores = map[string][]int{
	"d":  {2, 4, 8, 16, 32, 48, 64, 96, 128, 192},
	"e":  {2, 4, 8, 16, 20, 32, 48, 64, 96, 128, 192},
	"v3": {2, 4, 8, 16, 32, 48, 64}, // Dsv3 and Esv3 (no 20 or 96)
	"v4": {2, 4, 8, 16, 20, 32, 48, 64},
	"v5": {2, 4, 8, 16, 20, 32, 48, 64, 96},
	"v6": {2, 4, 8, 16, 20, 32, 48, 64, 96, 128, 192},
}

// azureLargeEGiB is the memory of the E sizes above 48 vCores.
var azureLargeEGiB = map[string]float64{
	"64s_v3": 432, "64ds_v4": 432, "64ds_v5": 512, "64ads_v6": 512, "64ds_v6": 512,
	"96ds_v5": 672, "96ads_v5": 672, "96ads_v6": 672, "96ds_v6": 768,
	"128ds_v6": 1024, "192ds_v6": 1832,
}

// AzureSKUMemoryBytes is the documented RAM of a flexible-server SKU name
// as ARM reports it (Standard_D4ds_v4); 0 when unknown.
func AzureSKUMemoryBytes(sku string) float64 {
	name := strings.ToLower(strings.TrimSpace(sku))
	if gib, ok := azureBurstableGiB[strings.TrimPrefix(name, "standard_")]; ok &&
		strings.HasPrefix(name, "standard_b") {
		return gib * gibF
	}
	m := azureSKUPattern.FindStringSubmatch(name)
	if m == nil {
		return 0
	}
	family, suffix := m[1], m[3]
	cores, err := strconv.Atoi(m[2])
	if err != nil || !offersVCores(family, suffix, cores) {
		return 0
	}
	if family == "d" {
		return float64(cores) * 4 * gibF
	}
	if cores <= 48 {
		return float64(cores) * 8 * gibF
	}
	return azureLargeEGiB[m[2]+suffix] * gibF
}

func offersVCores(family, suffix string, cores int) bool {
	generation := suffix[len(suffix)-2:]
	if family == "d" && cores == 20 {
		return false // General Purpose has no 20 vCore size
	}
	return containsInt(azureVCores[family], cores) &&
		containsInt(azureVCores[generation], cores)
}

func containsInt(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
