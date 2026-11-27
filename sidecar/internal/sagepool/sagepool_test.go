package sagepool_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/databricksauth"
	"github.com/pg-sage/sidecar/internal/sagepool"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
)

const (
	secret = "Zq7SagepoolSecret"
	// plainDSN names a local port nothing listens on (dialled only lazily).
	plainDSN = "postgres://sage:" + secret + "@127.0.0.1:1/sagepool" +
		"?sslmode=disable&application_name=other&connect_timeout=2"
	lakebaseHost     = "ep-sagepool-unit.database.example.com"
	lakebaseEndpoint = "projects/sagepool/branches/unit/endpoints/primary"
	// fleetMap names lakebaseHost only, as a fleet's SAGE_LAKEBASE_ENDPOINTS
	// does; other hosts stay ordinary PostgreSQL.
	fleetMap = `{"` + lakebaseHost + `": "` + lakebaseEndpoint + `"}`
)

// message is one typed protocol message as the server decodes it.
type message struct {
	kind       byte
	name, text string // a Parse's statement name; a Query's or Parse's text
}

// roundTrip builds a frontend with build over out, sends and flushes a
// startup message (as pgconn does), then msgs, and decodes out.
func roundTrip(build pgconn.BuildFrontendFunc, out *bytes.Buffer,
	msgs ...pgproto3.FrontendMessage) (map[string]string, []message, error) {
	fe := build(bytes.NewReader(nil), out)
	fe.Send(&pgproto3.StartupMessage{ProtocolVersion: pgproto3.ProtocolVersion30,
		Parameters: map[string]string{"user": "sage", "database": "sagepool"}})
	if err := fe.Flush(); err != nil {
		return nil, nil, fmt.Errorf("flush the startup message: %w", err)
	}
	for _, m := range msgs {
		fe.Send(m)
	}
	if err := fe.Flush(); err != nil {
		return nil, nil, fmt.Errorf("flush: %w", err)
	}
	return decode(out.Bytes())
}

// decode reads wire as the server does: the startup parameters, then the
// typed messages; a length that does not frame the bytes is an error.
func decode(wire []byte) (map[string]string, []message, error) {
	if len(wire) < 8 {
		return nil, nil, fmt.Errorf("%d bytes: no startup message", len(wire))
	}
	n := int(binary.BigEndian.Uint32(wire))
	var start pgproto3.StartupMessage
	if n < 8 || n > len(wire) || start.Decode(wire[4:n]) != nil {
		return nil, nil, fmt.Errorf("startup length %d in %d bytes", n, len(wire))
	}
	var out []message
	for rest := wire[n:]; len(rest) > 0; {
		size := 0
		if len(rest) >= 5 {
			size = 1 + int(binary.BigEndian.Uint32(rest[1:5]))
		}
		if size < 5 || size > len(rest) {
			return nil, nil, fmt.Errorf("bad framing with %d bytes left", len(rest))
		}
		m := message{kind: rest[0]}
		var err error
		switch body := rest[5:size]; m.kind {
		case 'Q':
			var q pgproto3.Query
			err = q.Decode(body) // the NUL must end the body: exact length
			m.text = q.String
		case 'P':
			var p pgproto3.Parse
			err = p.Decode(body)
			m.name, m.text = p.Name, p.Query
		}
		if err != nil {
			return nil, nil, fmt.Errorf("decode a %q message: %w", m.kind, err)
		}
		out, rest = append(out, m), rest[size:]
	}
	return start.Parameters, out, nil
}

// statements is roundTrip over a fresh buffer, failing t on any error.
func statements(t *testing.T, build pgconn.BuildFrontendFunc,
	msgs ...pgproto3.FrontendMessage) (map[string]string, []message) {
	t.Helper()
	params, got, err := roundTrip(build, &bytes.Buffer{}, msgs...)
	if err != nil {
		t.Fatalf("send through the configured frontend: %v", err)
	}
	if len(got) != len(msgs) {
		t.Fatalf("%d messages reached the wire, want %d", len(got), len(msgs))
	}
	return params, got
}

// assertOwn checks cfg opens pg_sage's own sessions: application_name
// pg_sage, the pins' AfterConnect, a frontend that tags after the first
// keyword, and no AfterNetConnect wrapper (a wrapped net.Conn turned SCRAM
// channel binding off).
func assertOwn(t *testing.T, cfg *pgxpool.Config) {
	t.Helper()
	if cfg == nil || cfg.ConnConfig == nil {
		t.Fatal("no pool config")
	}
	cc := cfg.ConnConfig
	if got := cc.RuntimeParams["application_name"]; got != selfmonitor.ApplicationName {
		t.Errorf("application_name = %q, want %q", got, selfmonitor.ApplicationName)
	}
	if cc.AfterConnect == nil {
		t.Error("AfterConnect is nil: sessions would not be pinned")
	}
	if cc.AfterNetConnect != nil {
		t.Error("AfterNetConnect is set: pgx sees no *tls.Conn and drops channel binding")
	}
	_, got := statements(t, cc.BuildFrontend, &pgproto3.Query{String: "SELECT 1"})
	if want := "SELECT " + selfmonitor.StatementTag + " 1"; got[0].text != want {
		t.Errorf("Query reached the wire as %q, want %q", got[0].text, want)
	}
}

// New keeps the target (the Lakebase path would clear the password) and
// the pool size, and does not connect: the first use reports the
// unreachable server as a connect error that carries no password.
func TestNew_OrdinaryDSNOpensPgSagesOwnPoolLazily(t *testing.T) {
	ordinaryPostgres(t)
	pool, err := sagepool.New(context.Background(), plainDSN+"&pool_max_conns=1")
	if err != nil || pool == nil {
		t.Fatalf("New = %v, %v; want a pool (it must not connect)", pool, err)
	}
	t.Cleanup(pool.Close)
	assertOwn(t, pool.Config())
	cc := pool.Config().ConnConfig
	if cc.Host != "127.0.0.1" || cc.Port != 1 || cc.Database != "sagepool" ||
		cc.Password != secret || pool.Config().MaxConns != 1 {
		t.Errorf("target %s:%d/%s, password kept %v, MaxConns %d; want 127.0.0.1:1/sagepool, "+
			"true, 1", cc.Host, cc.Port, cc.Database, cc.Password == secret,
			pool.Config().MaxConns)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err = pool.Ping(ctx)
	var ce *pgconn.ConnectError
	if !errors.As(err, &ce) {
		t.Fatalf("Ping on 127.0.0.1:1 = %T %v, want *pgconn.ConnectError", err, err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("connect error carries the password: %q", err)
	}
}

func TestConfigure_TagsEveryStatementAfterItsFirstKeyword(t *testing.T) {
	ordinaryPostgres(t)
	cfg := parse(t, plainDSN)
	if err := sagepool.Configure(context.Background(), cfg); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	tag := selfmonitor.StatementTag
	params, got := statements(t, cfg.ConnConfig.BuildFrontend,
		&pgproto3.Query{String: "SELECT 1"},
		&pgproto3.Query{String: tag + " SELECT 2"},
		&pgproto3.Query{String: "SELECT 3; UPDATE public.t SET a = 4"},
		&pgproto3.Query{String: "SELECT " + tag + " 5"},
		&pgproto3.Parse{Name: "s1", Query: "SELECT $1::int"},
		&pgproto3.Sync{})
	want := []message{
		{kind: 'Q', text: "SELECT " + tag + " 1"},
		{kind: 'Q', text: "SELECT " + tag + " 2"}, // a leading tag is moved
		{kind: 'Q', text: "SELECT " + tag + " 3; UPDATE " + tag + " public.t SET a = 4"},
		{kind: 'Q', text: "SELECT " + tag + " 5"}, // already placed: no second tag
		{kind: 'P', name: "s1", text: "SELECT " + tag + " $1::int"},
		{kind: 'S'},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("message %d reached the wire as %+v, want %+v", i+1, got[i], want[i])
		}
	}
	// The untyped startup message passes through unchanged.
	if params["user"] != "sage" || params["database"] != "sagepool" || len(params) != 2 {
		t.Errorf("startup parameters = %v, want user=sage database=sagepool", params)
	}
}

// Hooks set before Configure run first: the earlier BuildFrontend builds
// over the tagging writer, and the earlier AfterConnect's error fails the
// session before any pin runs (a pin on the nil connection would panic,
// which callHook reports as an error).
func TestConfigure_ChainsHooksAlreadySet(t *testing.T) {
	ordinaryPostgres(t)
	cfg := parse(t, plainDSN)
	builds, hooks := 0, 0
	var handed io.Writer
	cfg.ConnConfig.BuildFrontend = func(r io.Reader, w io.Writer) *pgproto3.Frontend {
		builds, handed = builds+1, w
		return pgproto3.NewFrontend(r, w)
	}
	hookErr := errors.New("proxy session setup refused")
	cfg.ConnConfig.AfterConnect = func(context.Context, *pgconn.PgConn) error {
		hooks++
		return hookErr
	}
	if err := sagepool.Configure(context.Background(), cfg); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	var out bytes.Buffer
	_, got, err := roundTrip(cfg.ConnConfig.BuildFrontend, &out,
		&pgproto3.Query{String: "SELECT 1"})
	if err != nil || len(got) != 1 || builds != 1 || handed == io.Writer(&out) {
		t.Fatalf("round trip: %d messages, err %v; the earlier BuildFrontend ran %d times "+
			"(want 1), on the raw writer %v", len(got), err, builds, handed == io.Writer(&out))
	}
	if want := "SELECT " + selfmonitor.StatementTag + " 1"; got[0].text != want {
		t.Errorf("chained frontend sent %q, want %q", got[0].text, want)
	}
	callHook := func() (err error) {
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("a pin ran before the earlier hook's error: %v", p)
			}
		}()
		return cfg.ConnConfig.AfterConnect(context.Background(), nil)
	}
	if err := callHook(); !errors.Is(err, hookErr) || hooks != 1 {
		t.Fatalf("AfterConnect = %v with the earlier hook run %d times; want its %v, once",
			err, hooks, hookErr)
	}
}

// State transition: configuring twice wraps the writer twice; a placed
// tag is never placed again, so each statement carries exactly one.
func TestConfigure_TwiceStillTagsOnce(t *testing.T) {
	ordinaryPostgres(t)
	cfg := parse(t, plainDSN)
	for i := range 2 {
		if err := sagepool.Configure(context.Background(), cfg); err != nil {
			t.Fatalf("Configure %d: %v", i+1, err)
		}
	}
	params, got := statements(t, cfg.ConnConfig.BuildFrontend,
		&pgproto3.Parse{Name: "s", Query: "DELETE FROM public.t WHERE id = $1"})
	want := "DELETE " + selfmonitor.StatementTag + " FROM public.t WHERE id = $1"
	if got[0].text != want || params["user"] != "sage" {
		t.Errorf("Parse sent as %q with startup %v, want %q with user=sage", got[0].text,
			params, want)
	}
}

// One pool's BuildFrontend builds every connection's frontend, often at
// once; each must tag its own stream with its own state.
func TestConfigure_ConcurrentFrontendsTagIndependently(t *testing.T) {
	ordinaryPostgres(t)
	cfg := parse(t, plainDSN)
	if err := sagepool.Configure(context.Background(), cfg); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	const n = 32
	texts := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			sql := fmt.Sprintf("SELECT %d AS c%d", i, i)
			_, got, err := roundTrip(cfg.ConnConfig.BuildFrontend, &bytes.Buffer{},
				&pgproto3.Query{String: sql})
			if errs[i] = err; err == nil && len(got) == 1 {
				texts[i] = got[0].text
			}
		})
	}
	wg.Wait()
	for i := range n {
		want := fmt.Sprintf("SELECT %s %d AS c%d", selfmonitor.StatementTag, i, i)
		if errs[i] != nil || texts[i] != want {
			t.Errorf("frontend %d sent %q (err %v), want %q", i, texts[i], errs[i], want)
		}
	}
}

// Every DSN pgx refuses is ErrUnparseableDSN itself, wrapping nothing a
// caller could print: pgx's error printed the string after a redaction that
// leaves half of a password holding '@' or ':', and quoted one cut at '/',
// '?' or '#' as the port. pool_max_conns=0 is refused here; 1 opens (above).
func TestNew_UnparseableDSNReturnsErrUnparseableDSNWithoutPassword(t *testing.T) {
	ordinaryPostgres(t)
	const one, two = "Zq7HalfOne", "Zq7HalfTwo"
	url := "postgres://sage:" + secret + "@127.0.0.1:5432/db"
	split := func(sep, port string) string {
		return "postgres://sage:" + one + sep + two + "@127.0.0.1:" + port + "/db"
	}
	for name, dsn := range map[string]string{
		"URL sslmode":           url + "?sslmode=bogus",
		"keyword/value sslmode": "host=127.0.0.1 user=sage password=" + secret + " sslmode=bogus",
		"URL port":              "postgres://sage:" + secret + "@127.0.0.1:notaport/db",
		"channel_binding":       url + "?channel_binding=bogus",
		"pool_max_conns 0":      url + "?pool_max_conns=0",
		"pool_max_conns text":   url + "?pool_max_conns=many",
		"@ in password":         split("@", "notaport"),
		": in password":         split(":", "notaport"),
		"/ in password":         split("/", "5432"),
		"? in password":         split("?", "5432"),
		"# in password":         split("#", "5432"),
	} {
		t.Run(name, func(t *testing.T) {
			pool, err := sagepool.New(context.Background(), dsn)
			if pool != nil {
				pool.Close()
			}
			cfg, parseErr := sagepool.ParseConfig(dsn)
			var pce *pgconn.ParseConfigError
			for fn, got := range map[string]error{"New": err, "ParseConfig": parseErr} {
				text := fmt.Sprint(got)
				leaks := strings.Contains(text, secret) || strings.Contains(text, one) ||
					strings.Contains(text, two)
				if leaks || pool != nil || cfg != nil || errors.As(got, &pce) ||
					!errors.Is(got, sagepool.ErrUnparseableDSN) ||
					text != sagepool.ErrUnparseableDSN.Error() {
					t.Errorf("%s = %T %q (pool %v, config %v, a password piece %v); want "+
						"sagepool.ErrUnparseableDSN alone", fn, got, text, pool != nil,
						cfg != nil, leaks)
				}
			}
		})
	}
}

// assertRefused requires New and Configure to refuse dsn with
// ErrUnverifiedTLS (permanent, password kept out) before any Databricks
// call: no credential is minted for a connection that could expose it.
func assertRefused(t *testing.T, ctx context.Context, dsn string) {
	t.Helper()
	pool, err := sagepool.New(ctx, dsn)
	if pool != nil {
		pool.Close()
		t.Error("New returned a pool for an unverified Lakebase connection")
	}
	cfgErr := sagepool.Configure(ctx, parse(t, dsn))
	for name, got := range map[string]error{"New": err, "Configure": cfgErr} {
		if !errors.Is(got, databricksauth.ErrUnverifiedTLS) {
			t.Errorf("%s error = %v, want databricksauth.ErrUnverifiedTLS", name, got)
			continue
		}
		if !errors.Is(got, databricksauth.ErrEndpointPermanent) {
			t.Errorf("%s error %v is not marked permanent (a retry repeats it)", name, got)
		}
		if strings.Contains(got.Error(), secret) {
			t.Errorf("%s error text carries the password", name)
		}
	}
}

func TestNewAndConfigure_RefuseUnverifiedTLSForLakebaseHost(t *testing.T) {
	for _, mode := range []string{"disable", "allow", "prefer", "require", "verify-ca"} {
		t.Run(mode, func(t *testing.T) {
			lakebaseEnv(t, "", fleetMap)
			assertRefused(t, context.Background(), lakebaseDSN(lakebaseHost, mode))
		})
	}
	t.Run("host case and trailing dot", func(t *testing.T) {
		lakebaseEnv(t, "", fleetMap)
		host := strings.ToUpper(lakebaseHost) + "."
		assertRefused(t, context.Background(), lakebaseDSN(host, "require"))
	})
	t.Run("single endpoint names every host", func(t *testing.T) {
		lakebaseEnv(t, lakebaseEndpoint, "")
		assertRefused(t, context.Background(), plainDSN)
	})
	// Boundary: a host the fleet map does not name stays ordinary
	// PostgreSQL whatever its sslmode, and keeps its password.
	t.Run("unmapped host", func(t *testing.T) {
		lakebaseEnv(t, "", fleetMap)
		pool, err := sagepool.New(context.Background(),
			lakebaseDSN("db.sagepool.example.com", "require"))
		if err != nil {
			t.Fatalf("New on an unmapped host: %v", err)
		}
		t.Cleanup(pool.Close)
		assertOwn(t, pool.Config())
		if pool.Config().ConnConfig.Password != secret {
			t.Error("the password was cleared: the host was treated as Lakebase")
		}
	})
}

func TestNew_LakebaseEnvironmentErrorsPropagate(t *testing.T) {
	for _, tc := range []struct{ name, one, many, want string }{
		{"malformed endpoint", "projects/sagepool", "",
			"projects/<id>/branches/<id>/endpoints/<id>"},
		{"both variables", lakebaseEndpoint, `{"h.example.com": "` + lakebaseEndpoint + `"}`,
			"set only one of SAGE_LAKEBASE_ENDPOINT and SAGE_LAKEBASE_ENDPOINTS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lakebaseEnv(t, tc.one, tc.many)
			pool, err := sagepool.New(context.Background(), plainDSN)
			if pool != nil {
				pool.Close()
				t.Fatal("New returned a pool under a refused Lakebase environment")
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("New error = %v, want one containing %q", err, tc.want)
			}
			if !errors.Is(err, databricksauth.ErrEndpointPermanent) {
				t.Errorf("error %v is not marked permanent", err)
			}
		})
	}
}

// A nil config is a caller bug: Configure must not panic, and must refuse
// it with ErrNoConfig, not return nil as if the sessions were pg_sage's own.
func TestConfigure_NilConfigDoesNotPanic(t *testing.T) {
	ordinaryPostgres(t)
	for name, cfg := range map[string]*pgxpool.Config{"nil config": nil, "nil ConnConfig": {}} {
		t.Run(name, func(t *testing.T) {
			var err error
			panicked := func() (p any) {
				defer func() { p = recover() }()
				err = sagepool.Configure(context.Background(), cfg)
				return nil
			}()
			if panicked != nil {
				t.Fatalf("Configure(ctx, %s) panicked: %v", name, panicked)
			}
			if !errors.Is(err, sagepool.ErrNoConfig) {
				t.Errorf("Configure(ctx, %s) = %v, want sagepool.ErrNoConfig", name, err)
			}
		})
	}
}

// A cancelled context does not stop New or Configure on ordinary
// PostgreSQL (the pool connects lazily; the first use reports it), and
// Configure keeps what its caller set. A Lakebase TLS refusal comes first.
func TestNewAndConfigure_CancelledContext(t *testing.T) {
	ordinaryPostgres(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pool, err := sagepool.New(ctx, plainDSN)
	if err != nil {
		t.Fatalf("New on a cancelled context: %v", err)
	}
	t.Cleanup(pool.Close)
	assertOwn(t, pool.Config())
	if err := pool.Ping(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Ping on the cancelled context = %v, want context.Canceled", err)
	}
	cfg := parse(t, plainDSN)
	cfg.MaxConns = 3
	if err := sagepool.Configure(ctx, cfg); err != nil {
		t.Fatalf("Configure on a cancelled context: %v", err)
	}
	assertOwn(t, cfg)
	if cfg.MaxConns != 3 || cfg.ConnConfig.Password != secret {
		t.Errorf("MaxConns %d, password kept %v; want the caller's 3, true", cfg.MaxConns,
			cfg.ConnConfig.Password == secret)
	}
	lakebaseEnv(t, "", fleetMap)
	assertRefused(t, ctx, lakebaseDSN(lakebaseHost, "require"))
}

// An empty DSN takes the libpq environment; pg_sage's name still wins
// over PGAPPNAME.
func TestNew_EmptyDSNUsesTheEnvironmentAndStaysOwn(t *testing.T) {
	ordinaryPostgres(t)
	for name, value := range map[string]string{"PGHOST": "127.0.0.1", "PGPORT": "1",
		"PGDATABASE": "sagepool_env", "PGSSLMODE": "disable", "PGAPPNAME": "psql",
		"PGSERVICE": "", "PGSSLROOTCERT": "", "PGSSLCERT": "", "PGSSLKEY": "",
		"PGSSLNEGOTIATION": "", "PGTARGETSESSIONATTRS": ""} {
		t.Setenv(name, value)
	}
	pool, err := sagepool.New(context.Background(), "")
	if err != nil {
		t.Fatalf("New(\"\"): %v", err)
	}
	t.Cleanup(pool.Close)
	cc := pool.Config().ConnConfig
	if cc.Host != "127.0.0.1" || cc.Port != 1 || cc.Database != "sagepool_env" {
		t.Errorf("target %s:%d/%s, want 127.0.0.1:1/sagepool_env", cc.Host, cc.Port,
			cc.Database)
	}
	assertOwn(t, pool.Config())
}
