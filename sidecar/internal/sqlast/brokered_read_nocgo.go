//go:build !cgo

package sqlast

// InspectBrokeredRead needs the libpg_query parser; without cgo it reports
// ErrUnavailable so the brokered read path refuses (fail closed).
func InspectBrokeredRead(string) (BrokeredRead, error) {
	return BrokeredRead{}, ErrUnavailable
}
