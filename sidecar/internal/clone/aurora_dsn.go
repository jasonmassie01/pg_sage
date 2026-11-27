package clone

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// An Aurora clone's DSN is the monitored DSN with the clone's endpoint in
// place of the source's (same users, copied data). Errors never quote a
// DSN: it may hold a password.

var (
	rdsClusterID    = regexp.MustCompile(`^[a-z](?:[a-z0-9]|-[a-z0-9]){0,62}$`)
	rdsClusterLabel = regexp.MustCompile(`^cluster-(ro-)?[a-z0-9]{6,20}$`)
)

// auroraClusterHost is the cluster and region an Aurora cluster endpoint
// (<cluster>.cluster-<id>.<region>.rds.amazonaws.com[.cn], or its
// cluster-ro- reader) names. Instance and custom endpoints, proxies and
// custom DNS name no cluster.
func auroraClusterHost(host string) (string, string, bool) {
	host = normHost(host)
	rest, ok := strings.CutSuffix(host, ".rds.amazonaws.com")
	if !ok {
		rest, ok = strings.CutSuffix(host, ".rds.amazonaws.com.cn")
	}
	if !ok {
		return "", "", false
	}
	labels := strings.Split(rest, ".")
	if len(labels) != 3 || !rdsClusterID.MatchString(labels[0]) ||
		!rdsClusterLabel.MatchString(labels[1]) || !auroraRegion.MatchString(labels[2]) {
		return "", "", false
	}
	return labels[0], labels[2], true
}

func normHost(h string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
}

func sameHost(a, b string) bool { return normHost(a) != "" && normHost(a) == normHost(b) }

// dsnHost is the one host a DSN names.
func dsnHost(dsn string) (string, error) {
	if isURLDSN(dsn) {
		u, err := parseURLDSN(dsn)
		if err != nil {
			return "", err
		}
		return u.Hostname(), nil
	}
	kv, err := parseKeywordDSN(dsn)
	if err != nil {
		return "", err
	}
	host, err := kv.host()
	return host, err
}

// auroraCloneDSN is dsn on host:port. dsn must name exactly prodHost, with
// no hostaddr or service that could route elsewhere; the clone's sslmode
// is at least require (it holds production data).
func auroraCloneDSN(dsn, prodHost, host string, port int32) (string, error) {
	if isURLDSN(dsn) {
		return urlCloneDSN(dsn, prodHost, host, port)
	}
	kv, err := parseKeywordDSN(dsn)
	if err != nil {
		return "", err
	}
	current, err := kv.host()
	if err != nil {
		return "", err
	}
	if !sameHost(current, prodHost) {
		return "", fmt.Errorf("%w: the monitored DSN names host %s, not %s",
			ErrInvalidAuroraConfig, current, prodHost)
	}
	kv.set("host", host)
	if port != 5432 || kv.has("port") {
		kv.set("port", strconv.Itoa(int(port)))
	}
	if weakSSLMode(kv.value("sslmode")) {
		kv.set("sslmode", "require")
	}
	return kv.String(), nil
}

func isURLDSN(dsn string) bool {
	return strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://")
}

func parseURLDSN(dsn string) (*url.URL, error) {
	u, err := url.Parse(dsn)
	if err != nil || u.Host == "" || strings.Contains(u.Host, ",") {
		return nil, fmt.Errorf("%w: the monitored DSN is not a postgres URL with one host",
			ErrInvalidAuroraConfig)
	}
	for _, k := range []string{"host", "hostaddr", "service"} {
		if u.Query().Has(k) {
			return nil, fmt.Errorf("%w: the monitored DSN sets %s in its query, which could "+
				"route a clone connection elsewhere", ErrInvalidAuroraConfig, k)
		}
	}
	return u, nil
}

func urlCloneDSN(dsn, prodHost, host string, port int32) (string, error) {
	u, err := parseURLDSN(dsn)
	if err != nil {
		return "", err
	}
	if !sameHost(u.Hostname(), prodHost) {
		return "", fmt.Errorf("%w: the monitored DSN names host %s, not %s",
			ErrInvalidAuroraConfig, u.Hostname(), prodHost)
	}
	u.Host = net.JoinHostPort(host, strconv.Itoa(int(port)))
	q := u.Query()
	if weakSSLMode(q.Get("sslmode")) {
		q.Set("sslmode", "require")
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

// weakSSLMode is a mode that allows a plaintext connection.
func weakSSLMode(mode string) bool {
	switch mode {
	case "", "disable", "allow", "prefer":
		return true
	}
	return false
}

// keywordDSN is a libpq keyword/value DSN as written, each value kept raw
// (quoted as given) so a rewrite changes only what it sets.
type keywordDSN []keywordPair

type keywordPair struct{ key, raw, value string }

func parseKeywordDSN(dsn string) (keywordDSN, error) {
	var out keywordDSN
	s := strings.TrimSpace(dsn)
	if s == "" {
		return nil, fmt.Errorf("%w: the monitored DSN is empty", ErrInvalidAuroraConfig)
	}
	for s != "" {
		key, rest, ok := strings.Cut(s, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" || strings.ContainsAny(key, " \t") {
			return nil, fmt.Errorf("%w: the monitored DSN is not key=value pairs",
				ErrInvalidAuroraConfig)
		}
		raw, value, rest, err := keywordValue(strings.TrimLeft(rest, " \t"))
		if err != nil {
			return nil, err
		}
		out = append(out, keywordPair{key: key, raw: raw, value: value})
		s = strings.TrimLeft(rest, " \t\r\n")
	}
	for _, k := range []string{"hostaddr", "service"} {
		if out.has(k) {
			return nil, fmt.Errorf("%w: the monitored DSN sets %s, which could route a "+
				"clone connection elsewhere", ErrInvalidAuroraConfig, k)
		}
	}
	return out, nil
}

// keywordValue reads one value: single-quoted with \ escapes, or a run of
// non-space characters with \ escapes.
func keywordValue(s string) (raw, value, rest string, err error) {
	quoted := strings.HasPrefix(s, "'")
	var b strings.Builder
	i := 0
	if quoted {
		i = 1
	}
	for ; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s):
			i++
			b.WriteByte(s[i])
		case quoted && c == '\'':
			return s[:i+1], b.String(), s[i+1:], nil
		case !quoted && (c == ' ' || c == '\t' || c == '\n' || c == '\r'):
			return s[:i], b.String(), s[i:], nil
		default:
			b.WriteByte(c)
		}
	}
	if quoted {
		return "", "", "", fmt.Errorf("%w: the monitored DSN has an unterminated quote",
			ErrInvalidAuroraConfig)
	}
	return s, b.String(), "", nil
}

func (kv keywordDSN) has(key string) bool {
	for _, p := range kv {
		if p.key == key {
			return true
		}
	}
	return false
}

// value is the last value of key (libpq: the last one wins).
func (kv keywordDSN) value(key string) string {
	v := ""
	for _, p := range kv {
		if p.key == key {
			v = p.value
		}
	}
	return v
}

func (kv keywordDSN) host() (string, error) {
	h := kv.value("host")
	if h == "" || strings.Contains(h, ",") {
		return "", fmt.Errorf("%w: the monitored DSN must name exactly one host",
			ErrInvalidAuroraConfig)
	}
	return h, nil
}

// set replaces every value of key with value (written unquoted: it is a
// host, port or sslmode), or appends key=value.
func (kv *keywordDSN) set(key, value string) {
	found := false
	for i := range *kv {
		if (*kv)[i].key == key {
			(*kv)[i].raw, (*kv)[i].value, found = value, value, true
		}
	}
	if !found {
		*kv = append(*kv, keywordPair{key: key, raw: value, value: value})
	}
}

func (kv keywordDSN) String() string {
	parts := make([]string, len(kv))
	for i, p := range kv {
		parts[i] = p.key + "=" + p.raw
	}
	return strings.Join(parts, " ")
}
