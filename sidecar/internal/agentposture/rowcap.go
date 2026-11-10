package agentposture

import "strconv"

// rowCap bounds one capped catalog read. The statement fetches one row
// past the limit, so reading that row shows the read was cut; the
// truncation is then reported through the detector's input.
type rowCap struct {
	in       Input
	arm      string
	limit    int
	read     int
	reported bool
}

// capRows bounds the detector's read arm to limit rows (0 when negative).
func (in Input) capRows(arm string, limit int) *rowCap {
	return &rowCap{in: in, arm: arm, limit: max(limit, 0)}
}

// fetch is the statement's LIMIT: one row past the cap.
func (c *rowCap) fetch() int { return c.limit + 1 }

// past counts one row read and reports whether it is past the limit; the
// first such row reports the truncation. The caller stops reading then.
func (c *rowCap) past() bool {
	c.read++
	if c.read <= c.limit {
		return false
	}
	if !c.reported {
		c.reported = true
		c.in.Truncated(c.arm, c.limit)
	}
	return true
}

// thousands formats n with comma thousands separators (50,000).
func thousands(n int) string {
	s, sign := strconv.Itoa(n), ""
	if n < 0 {
		s, sign = s[1:], "-"
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return sign + s
}
