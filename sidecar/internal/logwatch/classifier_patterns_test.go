package logwatch

import (
	"testing"
)

// No concurrent-access tests here: matching reads only the immutable
// pattern table, and Classify is already serialised by c.mu.

// postmasterEntry is a postmaster line as csvlog/jsonlog write it: no
// user, no database, backend_type "postmaster".
func postmasterEntry(msg string) LogEntry {
	return baseEntry(func(e *LogEntry) {
		e.User, e.Database, e.Application = "", "", ""
		e.ErrorLevel, e.SQLState = "LOG", "00000"
		e.BackendType = "postmaster"
		e.Message = msg
	})
}

func classifyOne(e LogEntry) (string, map[string]any) {
	sig := defaultClassifier().Classify(e)
	if sig == nil {
		return "", nil
	}
	return sig.ID, sig.Metrics
}

func TestCrash_RealPostmasterLinesMatchWithIdentifiers(t *testing.T) {
	cases := []struct {
		msg      string
		pid, sig int
		procType string
	}{
		{"server process (PID 108) was terminated by signal 9: Killed",
			108, 9, "server process"},
		{"client backend (PID 103) was terminated by signal 11: Segmentation fault",
			103, 11, "client backend"},
		{`background worker "logical replication launcher" (PID 77) ` +
			`was terminated by signal 6: Aborted`, 77, 6,
			`background worker "logical replication launcher"`},
	}
	for _, tc := range cases {
		id, m := classifyOne(postmasterEntry(tc.msg))
		if id != "log_panic_server_crash" {
			t.Fatalf("%q: signal = %q, want log_panic_server_crash", tc.msg, id)
		}
		if m["crashed_pid"] != tc.pid || m["signal"] != tc.sig {
			t.Errorf("%q: crashed_pid=%v signal=%v, want %d/%d",
				tc.msg, m["crashed_pid"], m["signal"], tc.pid, tc.sig)
		}
		if m["process_type"] != tc.procType {
			t.Errorf("%q: process_type = %v, want %q", tc.msg, m["process_type"], tc.procType)
		}
	}
}

// Without backend_type (older csvlog, provider sinks) a line with no user
// and no database is treated as the postmaster's.
func TestCrash_NoBackendTypeFallsBackToNoUserNoDatabase(t *testing.T) {
	e := postmasterEntry("server process (PID 42) was terminated by signal 9: Killed")
	e.BackendType = ""
	if id, _ := classifyOne(e); id != "log_panic_server_crash" {
		t.Fatalf("signal = %q, want log_panic_server_crash", id)
	}
	e.User = "app"
	if id, _ := classifyOne(e); id != "" {
		t.Fatalf("line with a user and no backend_type: signal = %q, want none", id)
	}
}

func TestCrash_LookalikesDoNotMatch(t *testing.T) {
	crash := "server process (PID 4242) was terminated by signal 9: Killed"
	client := func(level, state, msg string) LogEntry {
		return baseEntry(func(e *LogEntry) {
			e.ErrorLevel, e.SQLState, e.Message = level, state, msg
			e.BackendType = "client backend"
		})
	}
	cases := map[string]LogEntry{
		"client RAISE EXCEPTION": client("ERROR", "P0001", crash),
		"client RAISE WARNING":   client("WARNING", "01000", crash),
		"client RAISE LOG":       client("LOG", "00000", crash),
		"non-numeric PID": postmasterEntry(
			"server process (PID abc) was terminated by signal 9: Killed"),
		"no PID": postmasterEntry("server process was terminated by signal 9"),
		"archive command": postmasterEntry(
			"archive command was terminated by signal 9: Killed"),
		"exit code, not a signal": postmasterEntry(
			"server process (PID 12) exited with exit code 1"),
		"mid-message text": postmasterEntry(
			"note: server process (PID 12) was terminated by signal 9"),
	}
	for name, e := range cases {
		if id, _ := classifyOne(e); id == "log_panic_server_crash" {
			t.Errorf("%s: matched log_panic_server_crash", name)
		}
	}
}

func TestCrash_PostmasterAtWarningIsNotTheCrashLine(t *testing.T) {
	e := postmasterEntry("server process (PID 9) was terminated by signal 9: Killed")
	e.ErrorLevel = "WARNING"
	if id, _ := classifyOne(e); id == "log_panic_server_crash" {
		t.Fatal("crash text at WARNING matched; PostgreSQL logs child exits at LOG")
	}
}

func TestCrash_PANICLevelStillMatches(t *testing.T) {
	e := baseEntry(func(e *LogEntry) {
		e.ErrorLevel, e.Message = "PANIC", "could not write to file \"pg_wal/x\""
	})
	if id, _ := classifyOne(e); id != "log_panic_server_crash" {
		t.Fatalf("PANIC: signal = %q, want log_panic_server_crash", id)
	}
}

func TestWALGap_RealMessagesMatch(t *testing.T) {
	sender := baseEntry(func(e *LogEntry) {
		e.ErrorLevel, e.SQLState, e.BackendType = "ERROR", "58P01", "walsender"
		e.Message = "requested WAL segment 000000010000000000000001 has already been removed"
	})
	receiver := baseEntry(func(e *LogEntry) {
		e.ErrorLevel, e.SQLState, e.BackendType = "FATAL", "08P01", "walreceiver"
		e.User, e.Database = "", ""
		e.Message = "could not receive data from WAL stream: ERROR:  requested WAL " +
			"segment 0000000100000000000000A3 has already been removed"
	})
	for name, e := range map[string]LogEntry{"walsender": sender, "walreceiver": receiver} {
		id, m := classifyOne(e)
		if id != "log_wal_segment_removed" {
			t.Fatalf("%s: signal = %q, want log_wal_segment_removed", name, id)
		}
		if m["wal_segment"] == "" || m["wal_segment"] == nil {
			t.Errorf("%s: wal_segment metric missing", name)
		}
	}
	if _, m := classifyOne(receiver); m["wal_segment"] != "0000000100000000000000A3" {
		t.Errorf("wal_segment = %v, want 0000000100000000000000A3", m["wal_segment"])
	}
}

// The walreceiver re-raises the walsender's error under its own SQLSTATE,
// which is a generic XX000 when no errcode is set. The WAL gap must win
// over the internal-error rule, or a broken standby is a mere warning.
func TestWALGap_WalreceiverUnderXX000IsStillAWALGap(t *testing.T) {
	e := baseEntry(func(e *LogEntry) {
		e.ErrorLevel, e.SQLState, e.BackendType = "FATAL", "XX000", "walreceiver"
		e.User, e.Database = "", ""
		e.Message = "could not receive data from WAL stream: ERROR:  requested WAL " +
			"segment 0000000100000000000000A3 has already been removed"
	})
	id, m := classifyOne(e)
	if id != "log_wal_segment_removed" {
		t.Fatalf("signal = %q, want log_wal_segment_removed", id)
	}
	if m["wal_segment"] != "0000000100000000000000A3" {
		t.Errorf("wal_segment = %v, want 0000000100000000000000A3", m["wal_segment"])
	}
}

// PG14-17 name auxiliary processes with a capitalised "WAL ..." prefix;
// a SIGKILLed WAL writer or WAL receiver is a server crash too.
func TestCrash_AuxiliaryProcessNamesMatch(t *testing.T) {
	cases := []struct {
		msg, procType string
		pid           int
	}{
		{"WAL writer process (PID 61) was terminated by signal 9: Killed",
			"WAL writer process", 61},
		{"WAL receiver process (PID 62) was terminated by signal 9: Killed",
			"WAL receiver process", 62},
		{"checkpointer process (PID 63) was terminated by signal 9: Killed",
			"checkpointer process", 63},
	}
	for _, tc := range cases {
		id, m := classifyOne(postmasterEntry(tc.msg))
		if id != "log_panic_server_crash" {
			t.Fatalf("%q: signal = %q, want log_panic_server_crash", tc.msg, id)
		}
		if m["process_type"] != tc.procType || m["crashed_pid"] != tc.pid ||
			m["signal"] != 9 {
			t.Errorf("%q: metrics = %v", tc.msg, m)
		}
	}
}

func TestWALGap_LookalikesDoNotMatch(t *testing.T) {
	cases := map[string]LogEntry{
		"invented text without a segment": baseEntry(func(e *LogEntry) {
			e.ErrorLevel = "ERROR"
			e.Message = "requested WAL segment has already been removed"
		}),
		"client RAISE": baseEntry(func(e *LogEntry) {
			e.ErrorLevel, e.SQLState, e.BackendType = "ERROR", "P0001", "client backend"
			e.Message = "requested WAL segment 000000010000000000000001 has already been removed"
		}),
		"below ERROR": baseEntry(func(e *LogEntry) {
			e.ErrorLevel, e.BackendType = "LOG", "walsender"
			e.Message = "requested WAL segment 000000010000000000000001 has already been removed"
		}),
	}
	for name, e := range cases {
		if id, _ := classifyOne(e); id == "log_wal_segment_removed" {
			t.Errorf("%s: matched log_wal_segment_removed", name)
		}
	}
}

func TestSlotInvalidation_RealMessagesMatchAtLOG(t *testing.T) {
	cases := map[string]string{
		"pg14-15": `invalidating slot "corpus_slot" because its restart_lsn ` +
			`0/160000D8 exceeds max_slot_wal_keep_size`,
		"pg16+": `invalidating obsolete replication slot "corpus_slot"`,
	}
	for name, msg := range cases {
		e := postmasterEntry(msg)
		e.BackendType = "checkpointer"
		if !ShouldParseLine(e.ErrorLevel, e.Message) {
			t.Fatalf("%s: pre-filter drops the LOG line", name)
		}
		id, m := classifyOne(e)
		if id != "log_replication_slot_inactive" {
			t.Fatalf("%s: signal = %q, want log_replication_slot_inactive", name, id)
		}
		if m["slot_name"] != "corpus_slot" {
			t.Errorf("%s: slot_name = %v, want corpus_slot", name, m["slot_name"])
		}
	}
}

func TestSlotInvalidation_LookalikesDoNotMatch(t *testing.T) {
	cases := map[string]LogEntry{
		"invented inactive text": baseEntry(func(e *LogEntry) {
			e.ErrorLevel = "WARNING"
			e.Message = `replication slot "my_slot" is inactive and will be dropped`
		}),
		"client RAISE LOG": baseEntry(func(e *LogEntry) {
			e.ErrorLevel, e.BackendType = "LOG", "client backend"
			e.Message = `invalidating obsolete replication slot "x"`
		}),
		"unquoted slot": postmasterEntry("invalidating slot x because of reasons"),
	}
	for name, e := range cases {
		if id, _ := classifyOne(e); id == "log_replication_slot_inactive" {
			t.Errorf("%s: matched log_replication_slot_inactive", name)
		}
	}
}

func TestXXClass_SeverityBoundaries(t *testing.T) {
	cases := []struct {
		state, wantID, wantSeverity string
	}{
		{"XX000", "log_internal_error", "warning"},
		{"XX001", "log_data_corruption", "critical"},
		{"XX002", "log_data_corruption", "critical"},
		// An unknown future XX code fails closed to corruption.
		{"XX003", "log_data_corruption", "critical"},
	}
	for _, tc := range cases {
		sig := defaultClassifier().Classify(baseEntry(func(e *LogEntry) {
			e.ErrorLevel, e.SQLState = "ERROR", tc.state
			e.Message = "cache lookup failed for operator class 0"
		}))
		if sig == nil {
			t.Fatalf("%s: no signal", tc.state)
		}
		if sig.ID != tc.wantID || sig.Severity != tc.wantSeverity {
			t.Errorf("%s: got %s/%s, want %s/%s", tc.state, sig.ID,
				sig.Severity, tc.wantID, tc.wantSeverity)
		}
	}
}

func TestXX000_BelowWarningIsIgnored(t *testing.T) {
	e := baseEntry(func(e *LogEntry) {
		e.ErrorLevel, e.SQLState, e.Message = "LOG", "XX000", "cache lookup failed"
	})
	if id, _ := classifyOne(e); id != "" {
		t.Fatalf("XX000 at LOG: signal = %q, want none", id)
	}
}

func TestWraparoundOutage_RequiresSQLStateAndText(t *testing.T) {
	pg14 := `database is not accepting commands to avoid wraparound data loss ` +
		`in database "postgres"`
	pg17 := `database is not accepting commands that assign new transaction IDs ` +
		`to avoid wraparound data loss in database "appdb"`
	byOID := `database is not accepting commands to avoid wraparound data loss ` +
		`in database with OID 16384`
	entry := func(state, msg string) LogEntry {
		return baseEntry(func(e *LogEntry) {
			e.ErrorLevel, e.SQLState, e.Message = "ERROR", state, msg
		})
	}
	for msg, wantDB := range map[string]string{pg14: "postgres", pg17: "appdb", byOID: "16384"} {
		id, m := classifyOne(entry("54000", msg))
		if id != "log_wraparound_outage" {
			t.Fatalf("%q: signal = %q, want log_wraparound_outage", msg, id)
		}
		if m["wraparound_database"] != wantDB {
			t.Errorf("wraparound_database = %v, want %s", m["wraparound_database"], wantDB)
		}
	}
	if id, _ := classifyOne(entry("54000",
		"array size exceeds the maximum allowed (134217727)")); id != "" {
		t.Errorf("54000 without wraparound text: signal = %q, want none", id)
	}
	if id, _ := classifyOne(entry("P0001", pg17)); id == "log_wraparound_outage" {
		t.Error("wraparound text without 54000 (client RAISE) matched the outage")
	}
}

func TestMultixactLimits(t *testing.T) {
	cases := []struct {
		level, state, msg, want string
	}{
		{"WARNING", "01000", `database "postgres" must be vacuumed before 1234 ` +
			`more MultiXactIds are used`, "log_multixact_wraparound_warning"},
		{"WARNING", "01000", `database with OID 5 must be vacuumed before 1 more ` +
			`MultiXactId is used`, "log_multixact_wraparound_warning"},
		{"WARNING", "01000", `database with OID 5 must be vacuumed before 99 more ` +
			`multixact members are used`, "log_multixact_wraparound_warning"},
		{"ERROR", "54000", `database is not accepting commands that generate new ` +
			`MultiXactIds to avoid wraparound data loss in database "postgres"`,
			"log_multixact_wraparound_outage"},
		{"ERROR", "54000", `database is not accepting commands that assign new ` +
			`MultiXactIds to avoid wraparound data loss in database "postgres"`,
			"log_multixact_wraparound_outage"},
		{"ERROR", "54000", `multixact "members" limit exceeded`,
			"log_multixact_wraparound_outage"},
		{"ERROR", "P0001", `multixact "members" limit exceeded`, ""},
		{"LOG", "00000", `database "postgres" must be vacuumed before 1234 ` +
			`more MultiXactIds are used`, ""},
	}
	for _, tc := range cases {
		id, _ := classifyOne(baseEntry(func(e *LogEntry) {
			e.ErrorLevel, e.SQLState, e.Message = tc.level, tc.state, tc.msg
		}))
		if id != tc.want {
			t.Errorf("%s %s %q: signal = %q, want %q", tc.level, tc.state, tc.msg, id, tc.want)
		}
	}
}

func TestEmptyMessageAndSQLStateGiveNoSignal(t *testing.T) {
	for _, level := range []string{"LOG", "WARNING", "ERROR"} {
		e := baseEntry(func(e *LogEntry) { e.ErrorLevel, e.Message, e.SQLState = level, "", "" })
		if id, _ := classifyOne(e); id != "" {
			t.Errorf("%s with empty message and SQLSTATE: signal = %q", level, id)
		}
	}
	if sig := defaultClassifier().Classify(LogEntry{}); sig != nil {
		t.Errorf("zero LogEntry: signal = %s, want nil", sig.ID)
	}
}

func TestLogKeywords_IncludeInvalidating(t *testing.T) {
	if !ShouldParseLine("LOG", `invalidating slot "s" because its restart_lsn 0/1 `+
		`exceeds max_slot_wal_keep_size`) {
		t.Fatal("PG14-15 slot invalidation is dropped by the LOG pre-filter")
	}
}
