package main

import (
	"fmt"
	"net/http"
	"os"

	"github.com/pg-sage/sidecar/internal/config"
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
		Logf: func(format string, args ...any) { logWarn("tls", format, args...) },
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
