package config

// trustLevels is the one trust enum, ordered from least to most autonomy.
// The store, the API and cmd all validate and compare against it.
var trustLevels = [...]string{"observation", "advisory", "autonomous"}

// TrustLevels returns the accepted trust levels, least autonomous first.
// The slice is a copy; callers may modify it.
func TrustLevels() []string {
	return append([]string(nil), trustLevels[:]...)
}

// TrustRank orders trust levels: observation 0, advisory 1, autonomous 2.
// Any other string, including a differently cased level, ranks -1.
func TrustRank(level string) int {
	for rank, known := range trustLevels {
		if level == known {
			return rank
		}
	}
	return -1
}

// IsTrustLevel reports whether level is one of TrustLevels.
func IsTrustLevel(level string) bool {
	return TrustRank(level) >= 0
}
