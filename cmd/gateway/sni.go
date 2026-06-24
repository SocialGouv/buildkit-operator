package main

import (
	"encoding/binary"
	"errors"
	"io"
)

var errTruncated = errors.New("clienthello truncated")

// peekClientHelloSNI reads exactly the first TLS handshake record (the ClientHello) from r and
// returns the SNI host_name plus the raw bytes consumed. The gateway never decrypts: it replays
// these raw bytes to the backend so mTLS stays end-to-end to the daemon (client-cert auth intact).
func peekClientHelloSNI(r io.Reader) (sni string, raw []byte, err error) {
	hdr := make([]byte, 5) // record: type(1) | version(2) | length(2)
	if _, err = io.ReadFull(r, hdr); err != nil {
		return "", nil, err
	}
	if hdr[0] != 0x16 { // handshake
		return "", hdr, errors.New("not a TLS handshake record")
	}
	n := int(binary.BigEndian.Uint16(hdr[3:5]))
	if n == 0 || n > 1<<14+2048 {
		return "", hdr, errors.New("bad TLS record length")
	}
	body := make([]byte, n)
	if _, err = io.ReadFull(r, body); err != nil {
		return "", append(hdr, body...), err
	}
	sni, err = sniFromClientHello(body)
	return sni, append(hdr, body...), err
}

// sniFromClientHello extracts the server_name host_name from a ClientHello handshake body.
func sniFromClientHello(b []byte) (string, error) {
	// Handshake: msg_type(1)=ClientHello | length(3) | version(2) | random(32) | ...
	if len(b) < 4 || b[0] != 0x01 {
		return "", errors.New("not a ClientHello")
	}
	b = b[4:]
	if len(b) < 34 {
		return "", errTruncated
	}
	b = b[34:] // skip client_version(2) + random(32)

	for _, skip := range []int{1, 2, 1} { // session_id(len 1), cipher_suites(len 2), compression(len 1)
		if len(b) < skip {
			return "", errTruncated
		}
		var l int
		switch skip {
		case 1:
			l = int(b[0])
		case 2:
			l = int(binary.BigEndian.Uint16(b))
		}
		b = b[skip:]
		if len(b) < l {
			return "", errTruncated
		}
		b = b[l:]
	}

	if len(b) < 2 {
		return "", errors.New("no extensions")
	}
	extLen := int(binary.BigEndian.Uint16(b))
	b = b[2:]
	if len(b) < extLen {
		return "", errTruncated
	}
	b = b[:extLen]
	for len(b) >= 4 {
		etype := binary.BigEndian.Uint16(b)
		elen := int(binary.BigEndian.Uint16(b[2:]))
		b = b[4:]
		if len(b) < elen {
			return "", errTruncated
		}
		ext := b[:elen]
		b = b[elen:]
		if etype != 0x0000 { // server_name extension
			continue
		}
		if len(ext) < 2 { // server_name_list length
			return "", errTruncated
		}
		ext = ext[2:]
		for len(ext) >= 3 {
			nameType := ext[0]
			nameLen := int(binary.BigEndian.Uint16(ext[1:]))
			ext = ext[3:]
			if len(ext) < nameLen {
				return "", errTruncated
			}
			if nameType == 0 { // host_name
				return string(ext[:nameLen]), nil
			}
			ext = ext[nameLen:]
		}
	}
	return "", errors.New("no SNI in ClientHello")
}
