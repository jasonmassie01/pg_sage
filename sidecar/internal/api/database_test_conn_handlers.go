package api

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/pg-sage/sidecar/internal/store"
)

func testManagedDBHandler(
	deps *DatabaseDeps,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			jsonError(w, "invalid database ID", http.StatusBadRequest)
			return
		}
		req, hasPreviewBody, ok := decodeManagedTestBody(w, r)
		if !ok {
			return
		}
		rec, err := deps.Store.Get(r.Context(), id)
		if err != nil {
			jsonError(w, "database not found", http.StatusNotFound)
			return
		}
		connStr, ok := managedTestConnString(w, r, deps, id, rec, &req, hasPreviewBody)
		if !ok {
			return
		}
		result := testFromConnStringAtIP(r.Context(), connStr, req.resolvedIP)
		result.ActionIdentity = testManagedActionIdentity(r.Context(), deps, id, req,
			hasPreviewBody, connStr)
		jsonResponse(w, result)
	}
}

// decodeManagedTestBody reads an optional preview body: none tests the
// stored record, one tests the edited form.
func decodeManagedTestBody(
	w http.ResponseWriter, r *http.Request,
) (dbCreateRequest, bool, bool) {
	var req dbCreateRequest
	hasPreviewBody := false
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			if !errors.Is(err, io.EOF) {
				jsonError(w, "invalid request body", http.StatusBadRequest)
				return req, false, false
			}
		} else {
			hasPreviewBody = true
		}
	}
	return req, hasPreviewBody, true
}

// managedTestConnString picks what to test: the preview (falling back to
// the running or stored password), the running fleet connection, or the
// stored record.
func managedTestConnString(
	w http.ResponseWriter, r *http.Request, deps *DatabaseDeps, id int,
	rec *store.DatabaseRecord, req *dbCreateRequest, hasPreviewBody bool,
) (string, bool) {
	fleetConnStr, fleetPassword, fleetFound :=
		fleetManagedDBConnection(deps, id, rec.Name)
	if hasPreviewBody {
		return previewTestConnString(w, r, deps, id, rec, req, fleetPassword)
	}
	if fleetFound {
		return fleetConnStr, true
	}
	connStr, err := deps.Store.GetConnectionString(
		r.Context(), id)
	if err != nil {
		internalError(w, r,
			"read database credentials", err)
		return "", false
	}
	return connStr, true
}

func previewTestConnString(
	w http.ResponseWriter, r *http.Request, deps *DatabaseDeps, id int,
	rec *store.DatabaseRecord, req *dbCreateRequest, fleetPassword string,
) (string, bool) {
	if sameManagedHost(req.Host, rec.Host) {
		applyManagedTestDefaults(req)
	} else {
		if !applySafePreviewHost(w, req) {
			return "", false
		}
	}
	fallbackPassword := fleetPassword
	if req.Password == "" && fallbackPassword == "" {
		storedConnStr, storeErr := deps.Store.GetConnectionString(
			r.Context(), id)
		if storeErr != nil {
			internalError(w, r,
				"read database credentials", storeErr)
			return "", false
		}
		if parsed, parseErr := url.Parse(
			storedConnStr); parseErr == nil {
			fallbackPassword, _ = parsed.User.Password()
		}
	}
	return buildManagedTestConnString(
		*req, fallbackPassword), true
}

func fleetManagedDBConnection(
	deps *DatabaseDeps, id int, name string,
) (connStr string, password string, ok bool) {
	if deps == nil || deps.Fleet == nil {
		return "", "", false
	}
	inst := deps.Fleet.GetInstanceByDatabaseID(id)
	if inst == nil && name != "" {
		inst = deps.Fleet.GetInstance(name)
	}
	if inst == nil {
		return "", "", false
	}
	if inst.Pool != nil {
		connection := inst.Pool.Config().ConnConfig
		return connection.ConnString(), connection.Password, true
	}
	cfg := inst.ConfigSnapshot()
	return cfg.ConnString(), cfg.Password, true
}

func testConnectionPreviewHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req dbCreateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid request body",
				http.StatusBadRequest)
			return
		}

		// SSRF protection: reject private/internal IPs,
		// cloud metadata endpoints, and unresolvable hosts.
		// Resolve ONCE here and connect to the resolved IP to
		// prevent DNS-rebinding TOCTOU (a hostname with a short
		// TTL could resolve to a public IP during the check and
		// a private IP during pgx.Connect otherwise).
		if !applySafePreviewHost(w, &req) {
			return
		}
		u := buildManagedTestURL(req, req.Password)
		result := testFromConnStringAtIP(r.Context(), u.String(), req.resolvedIP)
		result.ActionIdentity = testRequestedActionIdentity(r.Context(), req, u.String())
		jsonResponse(w, result)
	}
}

func applySafePreviewHost(
	w http.ResponseWriter, req *dbCreateRequest,
) bool {
	safeIP, res := resolveSafeHost(req.Host)
	switch res {
	case hostBlocked:
		jsonError(w,
			"connections to private/internal addresses "+
				"are not allowed",
			http.StatusBadRequest)
		return false
	case hostDNSFailed:
		jsonError(w,
			"could not resolve hostname",
			http.StatusBadRequest)
		return false
	}

	// Preserve the logical hostname for endpoint authorization and TLS SNI.
	// The connection helper uses only this already-validated IP for dialing.
	req.resolvedIP = safeIP
	applyManagedTestDefaults(req)
	return true
}

func applyManagedTestDefaults(req *dbCreateRequest) {
	if req.Port == 0 {
		req.Port = 5432
	}
	if req.SSLMode == "" {
		req.SSLMode = "require"
	}
}

func sameManagedHost(incoming, stored string) bool {
	return strings.EqualFold(
		strings.TrimSpace(incoming),
		strings.TrimSpace(stored),
	)
}

func buildManagedTestConnString(
	req dbCreateRequest, fallbackPassword string,
) string {
	password := req.Password
	if password == "" {
		password = fallbackPassword
	}
	return buildManagedTestURL(req, password).String()
}

func buildManagedTestURL(
	req dbCreateRequest, password string,
) *url.URL {
	ssl := req.SSLMode
	if ssl == "" {
		ssl = "require"
	}
	port := req.Port
	if port == 0 {
		port = 5432
	}
	return &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(req.Username, password),
		Host:   net.JoinHostPort(req.Host, strconv.Itoa(port)),
		Path:   req.DatabaseName,
		RawQuery: url.Values{
			"sslmode":         {ssl},
			"connect_timeout": {"10"},
		}.Encode(),
	}
}

// hostCheckResult describes why a host was blocked.
type hostCheckResult int

const (
	hostAllowed   hostCheckResult = iota
	hostBlocked                   // private/internal IP
	hostDNSFailed                 // DNS resolution failed
)

// resolveSafeHost validates a hostname for SSRF and returns the
// specific IP that the caller should actually connect to. For a
// literal IP input it returns the input unchanged. For a hostname
// it resolves DNS once, blocks if any resolved IP is private/loopback/
// link-local, and otherwise returns the first allowed IP string. This
// eliminates the DNS-rebinding TOCTOU window between the safety check
// and pgx.Connect's own resolution.
func resolveSafeHost(host string) (string, hostCheckResult) {
	if host == "169.254.169.254" ||
		host == "metadata.google.internal" {
		return "", hostBlocked
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() ||
			ip.IsLinkLocalUnicast() ||
			ip.IsLinkLocalMulticast() {
			return "", hostBlocked
		}
		return host, hostAllowed
	}
	addrs, err := net.LookupHost(host)
	if err != nil || len(addrs) == 0 {
		return "", hostDNSFailed
	}
	var firstAllowed string
	for _, addr := range addrs {
		resolved := net.ParseIP(addr)
		if resolved == nil {
			continue
		}
		if resolved.IsLoopback() || resolved.IsPrivate() ||
			resolved.IsLinkLocalUnicast() ||
			resolved.IsLinkLocalMulticast() {
			return "", hostBlocked
		}
		if firstAllowed == "" {
			firstAllowed = addr
		}
	}
	if firstAllowed == "" {
		return "", hostDNSFailed
	}
	return firstAllowed, hostAllowed
}
