// Copyright 2023 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package eth

import "github.com/ethereum/go-ethereum/metrics"

// meters stores ingress and egress handshake meters.
var meters bidirectionalMeters

// bidirectionalMeters stores ingress and egress handshake meters.
type bidirectionalMeters struct {
	ingress *hsMeters
	egress  *hsMeters
}

// get returns the corresponding meter depending if ingress or egress is
// desired.
func (h *bidirectionalMeters) get(ingress bool) *hsMeters {
	if ingress {
		return h.ingress
	}
	return h.egress
}

// hsMeters is a collection of meters which track metrics related to the
// eth subprotocol handshake.
type hsMeters struct {
	// peerError measures the number of errors related to incorrect peer
	// behaviour, such as invalid message code, size, encoding, etc.
	peerError *metrics.Meter

	// timeoutError measures the number of timeouts.
	timeoutError *metrics.Meter

	// networkIDMismatch measures the number of network id mismatch errors.
	networkIDMismatch *metrics.Meter

	// protocolVersionMismatch measures the number of differing protocol
	// versions.
	protocolVersionMismatch *metrics.Meter

	// genesisMismatch measures the number of differing genesises.
	genesisMismatch *metrics.Meter

	// forkidRejected measures the number of differing forkids.
	forkidRejected *metrics.Meter
}

// newHandshakeMeters registers and returns handshake meters for the given
// base.
func newHandshakeMeters(base string) *hsMeters {
	return &hsMeters{
		peerError:               metrics.NewRegisteredMeter(base+"error/peer", nil),
		timeoutError:            metrics.NewRegisteredMeter(base+"error/timeout", nil),
		networkIDMismatch:       metrics.NewRegisteredMeter(base+"error/network", nil),
		protocolVersionMismatch: metrics.NewRegisteredMeter(base+"error/version", nil),
		genesisMismatch:         metrics.NewRegisteredMeter(base+"error/genesis", nil),
		forkidRejected:          metrics.NewRegisteredMeter(base+"error/forkid", nil),
	}
}

// A.37 XDC capability meters: tally peers by advertised state scheme +
// commit-stamp populated-ness so the operator can see at a glance whether
// the gate is working (e.g. expect mostly HBSS counts on Apothem, a small
// "legacy" tail for pre-A.37 fleet members). Wired in readStatus62 once
// per successful handshake. Refs #844 (A.37).
var (
	handshakeXDCCapsHBSS    = metrics.NewRegisteredMeter("eth/protocols/eth/handshake/xdccaps/hbss", nil)
	handshakeXDCCapsPBSS    = metrics.NewRegisteredMeter("eth/protocols/eth/handshake/xdccaps/pbss", nil)
	handshakeXDCCapsLegacy  = metrics.NewRegisteredMeter("eth/protocols/eth/handshake/xdccaps/legacy", nil)
	handshakeXDCCapsUnknown = metrics.NewRegisteredMeter("eth/protocols/eth/handshake/xdccaps/unknown", nil)
)

// markXDCCaps increments the appropriate XDC-caps meter once per handshake.
// A peer is "legacy" if it advertised a scheme but with an all-zero commit
// prefix (older build with caps support but pre-commit-stamp); "unknown" if
// it omitted the trailer entirely (pre-A.37 binary). Refs #844 (A.37).
func markXDCCaps(c XDCCaps) {
	if !metrics.Enabled() {
		return
	}
	switch c.Scheme {
	case XDCStateSchemeHBSS:
		handshakeXDCCapsHBSS.Mark(1)
	case XDCStateSchemePBSS:
		handshakeXDCCapsPBSS.Mark(1)
	default:
		if c.CommitPrefix == ([8]byte{}) {
			handshakeXDCCapsUnknown.Mark(1)
		} else {
			handshakeXDCCapsLegacy.Mark(1)
		}
	}
}

func init() {
	meters = bidirectionalMeters{
		ingress: newHandshakeMeters("eth/protocols/eth/ingress/handshake/"),
		egress:  newHandshakeMeters("eth/protocols/eth/egress/handshake/"),
	}
}
