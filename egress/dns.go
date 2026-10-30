package egress

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
)

// SyntheticDNS answers every A query with the gateway's address and every other query with no records, so a VM
// resolves any name to herd-egress and nothing it asks leaves the host (no DNS tunnel). Which names are reachable
// is decided at the gateway, by name, when the VM connects.
func SyntheticDNS(gateway netip.Addr) func([]byte) ([]byte, error) {
	a4 := gateway.Unmap().As4()
	return func(q []byte) ([]byte, error) {
		if len(q) < 12 {
			return nil, errors.New("dns: short message")
		}
		flags := binary.BigEndian.Uint16(q[2:4])
		if flags&0x8000 != 0 {
			return nil, errors.New("dns: not a query")
		}
		opcode := (flags >> 11) & 0xf
		qd := binary.BigEndian.Uint16(q[4:6])
		resp := make([]byte, 12, 512)
		copy(resp[0:2], q[0:2])
		rflags := uint16(0x8000) | opcode<<11 | 0x0400 | flags&0x0100 // QR, opcode, AA, RD
		if opcode != 0 || qd != 1 {
			binary.BigEndian.PutUint16(resp[2:4], rflags|4) // NOTIMP
			return resp, nil
		}
		// One question: QNAME (uncompressed labels), QTYPE, QCLASS.
		i := 12
		for {
			if i >= len(q) {
				return nil, errors.New("dns: truncated name")
			}
			l := int(q[i])
			if l == 0 {
				i++
				break
			}
			if l&0xc0 != 0 {
				return nil, errors.New("dns: compressed question name")
			}
			i += 1 + l
		}
		if i+4 > len(q) {
			return nil, errors.New("dns: truncated question")
		}
		question := q[12 : i+4]
		qtype := binary.BigEndian.Uint16(q[i : i+2])
		qclass := binary.BigEndian.Uint16(q[i+2 : i+4])
		binary.BigEndian.PutUint16(resp[2:4], rflags)
		binary.BigEndian.PutUint16(resp[4:6], 1)
		resp = append(resp, question...)
		if qtype == 1 && qclass == 1 && gateway.Unmap().Is4() {
			binary.BigEndian.PutUint16(resp[6:8], 1)
			resp = append(resp, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 5, 0, 4)
			resp = append(resp, a4[:]...)
		}
		return resp, nil
	}
}

// ServeDNS answers queries on pc with answer until ctx ends or pc fails.
func ServeDNS(ctx context.Context, pc net.PacketConn, answer func([]byte) ([]byte, error)) error {
	stop := context.AfterFunc(ctx, func() { _ = pc.Close() })
	defer stop()
	buf := make([]byte, 1500)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		resp, err := answer(buf[:n])
		if err != nil {
			continue
		}
		_, _ = pc.WriteTo(resp, from)
	}
}
