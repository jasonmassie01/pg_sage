package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/metricsreg"
	"github.com/pg-sage/sidecar/internal/servertls"
)

// apiTLS serves the API certificate when SAGE_TLS_CERT and SAGE_TLS_KEY are
// set (CG-02); nil means plain HTTP.
var apiTLS *servertls.Reloader

// loadAPITLSOrExit loads the TLS pair before anything connects or listens,
// so a bad pair stops startup with the file at fault instead of falling
// back to plain HTTP.
func loadAPITLSOrExit() {
	reloader, err := newAPITLS(cfg)
	if err != nil {
		logError("startup", "TLS: %v", err)
		os.Exit(1)
	}
	apiTLS = reloader
}

func newAPITLS(c *config.Config) (*servertls.Reloader, error) {
	if c == nil || !c.TLSEnabled() {
		return nil, nil
	}
	reloader, err := servertls.New(c.TLSCert, c.TLSKey, servertls.Options{
		Logf:   func(format string, args ...any) { logWarn("tls", format, args...) },
		Errorf: func(format string, args ...any) { logError("tls", format, args...) },
	})
	if err != nil {
		return nil, err
	}
	logInfo("startup", "API TLS enabled: minimum TLS 1.2, certificate %s, "+
		"reloaded when the files change", c.TLSCert)
	return reloader, nil
}

// listenAPI serves srv over TLS when a reloader is set, else plain HTTP.
func listenAPI(srv *http.Server, reloader *servertls.Reloader) error {
	if reloader == nil {
		return srv.ListenAndServe()
	}
	srv.TLSConfig = reloader.Config()
	// Empty file names: the certificate comes from TLSConfig.GetCertificate.
	return srv.ListenAndServeTLS("", "")
}

// apiListenLog is the "listening on" line, marking a TLS listener.
func apiListenLog(addr string, reloader *servertls.Reloader) string {
	if reloader == nil {
		return "listening on " + addr
	}
	return fmt.Sprintf("listening on %s (TLS)", addr)
}

// tlsExpiryCheckInterval is how often a never-rotated certificate's expiry
// is re-checked, so it is reported even when no reload ever happens.
const tlsExpiryCheckInterval = time.Hour

// startTLSExpiryWatch checks the serving certificate's expiry every
// interval until ctx ends. A nil reloader (plain HTTP) starts nothing.
func startTLSExpiryWatch(ctx context.Context, reloader *servertls.Reloader,
	interval time.Duration) {
	if reloader == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				reloader.CheckExpiry()
			}
		}
	}()
}

// apiTLSExpiringFunc feeds /ready its tls_expiring detail; nil (no detail)
// when the API serves plain HTTP.
func apiTLSExpiringFunc(reloader *servertls.Reloader) func() bool {
	if reloader == nil {
		return nil
	}
	return reloader.Expiring
}

// writeTLSMetrics exposes the serving certificate's expiry and the failed
// reload count. Nothing is written when the API serves plain HTTP.
func writeTLSMetrics(b *strings.Builder, reloader *servertls.Reloader) {
	if reloader == nil {
		return
	}
	metricsreg.Header(b, "pg_sage_api_tls_cert_not_after_seconds")
	fmt.Fprintf(b, "pg_sage_api_tls_cert_not_after_seconds %d\n\n", reloader.NotAfter().Unix())
	metricsreg.Header(b, "pg_sage_api_tls_reload_failures_total")
	fmt.Fprintf(b, "pg_sage_api_tls_reload_failures_total %d\n\n", reloader.ReloadFailures())
}
