//go:build !unix

package gvstack

import (
	"errors"
	"net"
	"os"
)

func socketPair() (net.Conn, *os.File, error) {
	return nil, nil, errors.New("datagram socket pairs need a unix host")
}
