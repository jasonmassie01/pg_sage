package config

import "fmt"

// TLSEnabled reports whether the API serves TLS from SAGE_TLS_CERT and
// SAGE_TLS_KEY (CG-02).
func (c *Config) TLSEnabled() bool {
	return c.TLSCert != "" && c.TLSKey != ""
}

// validateTLS rejects half a pair: serving plain HTTP when the operator
// asked for TLS would be a silent downgrade. The files themselves are
// checked when the listener starts (internal/servertls).
func (c *Config) validateTLS() error {
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return fmt.Errorf("SAGE_TLS_CERT and SAGE_TLS_KEY must be set together "+
			"(cert set: %t, key set: %t)", c.TLSCert != "", c.TLSKey != "")
	}
	return nil
}
