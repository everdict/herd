package auth

import (
	"crypto/tls"
	"fmt"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
)

// ServerTLS is the TLS of a herd listener (ADR 0001 A-60): it presents src's current X.509-SVID, and asks for a
// client certificate without requiring or verifying one, so a JWT-SVID bearer works over it as well. The handshake
// proves a client holds its certificate's key; the authenticator verifies the chain on every call, against the
// bundle src keeps current.
func ServerTLS(src Source) *tls.Config {
	c := tlsconfig.TLSServerConfig(src)
	c.ClientAuth = tls.RequestClientCert
	c.MinVersion = tls.VersionTLS12
	return c
}

// ClientTLS dials a herd component as src: it presents src's X.509-SVID and accepts only a server whose X.509-SVID is
// server, verified against src's bundle for the server's trust domain.
func ClientTLS(src Source, server spiffeid.ID) (*tls.Config, error) {
	self, err := Self(src)
	if err != nil {
		return nil, fmt.Errorf("auth: no X.509-SVID to dial %s with: %w", server, err)
	}
	c := tlsconfig.MTLSClientConfig(src, onlyDomain{td: self.TrustDomain(), src: src}, tlsconfig.AuthorizeID(server))
	c.MinVersion = tls.VersionTLS12
	return c, nil
}
