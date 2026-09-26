package logwatch

import (
	"fmt"
	"strings"
	"time"
)

// pgLogTimeLayout is PostgreSQL's log_time layout ("%Y-%m-%d
// %H:%M:%S.mmm") shared by csvlog and jsonlog; a zone follows it.
const pgLogTimeLayout = "2006-01-02 15:04:05.999"

// numericZoneLayouts cover zones tzdata renders without an abbreviation
// ("+05", "-0330", "+05:30").
var numericZoneLayouts = []string{"-07", "-0700", "-07:00"}

// zoneAbbreviations maps unambiguous tzdata abbreviations to UTC offsets
// in seconds. Go's time.Parse silently treats any abbreviation unknown to
// the sidecar's local zone as UTC, which skews log timestamps by hours
// (G1-B24). Ambiguous abbreviations (CST, IST, ...) are deliberately
// absent: they are rejected so an operator sets log_timezone explicitly.
var zoneAbbreviations = map[string]int{
	"UTC": 0, "UCT": 0, "GMT": 0, "Z": 0, "WET": 0,
	"WEST": 1 * 3600, "BST": 1 * 3600, "CET": 1 * 3600, "MET": 1 * 3600,
	"WAT": 1 * 3600, "CEST": 2 * 3600, "MEST": 2 * 3600, "EET": 2 * 3600,
	"CAT": 2 * 3600, "SAST": 2 * 3600, "EEST": 3 * 3600, "MSK": 3 * 3600,
	"EAT": 3 * 3600, "PKT": 5 * 3600, "WIB": 7 * 3600, "HKT": 8 * 3600,
	"AWST": 8 * 3600, "JST": 9 * 3600, "KST": 9 * 3600,
	"ACST": 9*3600 + 1800, "ACDT": 10*3600 + 1800, "AEST": 10 * 3600,
	"AEDT": 11 * 3600, "ChST": 10 * 3600, "NZST": 12 * 3600, "NZDT": 13 * 3600,
	"NST": -(3*3600 + 1800), "NDT": -(2*3600 + 1800), "AST": -4 * 3600,
	"ADT": -3 * 3600, "EST": -5 * 3600, "EDT": -4 * 3600, "CDT": -5 * 3600,
	"MST": -7 * 3600, "MDT": -6 * 3600, "PST": -8 * 3600, "PDT": -7 * 3600,
	"AKST": -9 * 3600, "AKDT": -8 * 3600, "HST": -10 * 3600, "HDT": -9 * 3600,
	"SST": -11 * 3600,
}

// parseCSVTimestamp parses a csvlog log_time field.
func parseCSVTimestamp(s string) (time.Time, error) {
	return parsePGTimestamp(s)
}

// parsePGTimestamp parses "YYYY-MM-DD HH:MM:SS.mmm ZONE" as written by
// PostgreSQL's csvlog and jsonlog and returns it in UTC. The zone is
// either a numeric offset or a known abbreviation; anything else is an
// error rather than a guess.
func parsePGTimestamp(s string) (time.Time, error) {
	for _, zl := range numericZoneLayouts {
		if t, err := time.Parse(pgLogTimeLayout+zl, s); err == nil {
			return t.UTC(), nil
		}
	}
	idx := strings.LastIndexByte(s, ' ')
	if idx <= 0 || idx == len(s)-1 {
		return time.Time{}, fmt.Errorf("timestamp %q has no time zone", s)
	}
	clock, zone := s[:idx], s[idx+1:]
	if zone[0] == '+' || zone[0] == '-' {
		for _, zl := range numericZoneLayouts {
			if t, err := time.Parse(pgLogTimeLayout+" "+zl, s); err == nil {
				return t.UTC(), nil
			}
		}
		return time.Time{}, fmt.Errorf("invalid numeric time zone %q", zone)
	}
	offset, ok := zoneAbbreviations[zone]
	if !ok {
		return time.Time{}, fmt.Errorf(
			"unknown time zone abbreviation %q; set log_timezone to UTC "+
				"or a zone with a numeric/unambiguous abbreviation", zone)
	}
	t, err := time.ParseInLocation(pgLogTimeLayout, clock, time.FixedZone(zone, offset))
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}
