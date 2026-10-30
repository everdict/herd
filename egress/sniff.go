package egress

import (
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"time"
)

var errSniffed = errors.New("egress: client hello read")

// readClientHello reads a TLS ClientHello from r with crypto/tls's own parser and returns its SNI and the bytes it
// consumed, which the caller replays to whoever handles the connection next.
func readClientHello(r io.Reader) (string, []byte, error) {
	var raw bytes.Buffer
	var sni string
	seen := false
	c := tls.Server(sniffConn{r: io.TeeReader(io.LimitReader(r, 64<<10), &raw)}, &tls.Config{
		GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
			sni, seen = h.ServerName, true
			return nil, errSniffed
		},
	})
	err := c.Handshake()
	if !seen {
		if err == nil {
			err = errors.New("handshake ended without a ClientHello")
		}
		return "", nil, err
	}
	return sni, raw.Bytes(), nil
}

// sniffConn is read-only: crypto/tls's alert on the aborted handshake goes nowhere.
type sniffConn struct{ r io.Reader }

func (s sniffConn) Read(p []byte) (int, error)     { return s.r.Read(p) }
func (sniffConn) Write(p []byte) (int, error)      { return len(p), nil }
func (sniffConn) Close() error                     { return nil }
func (sniffConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (sniffConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (sniffConn) SetDeadline(time.Time) error      { return nil }
func (sniffConn) SetReadDeadline(time.Time) error  { return nil }
func (sniffConn) SetWriteDeadline(time.Time) error { return nil }
