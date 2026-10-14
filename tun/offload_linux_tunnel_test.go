/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2017-2025 WireGuard LLC. All Rights Reserved.
 */

package tun

import (
	"encoding/binary"
	"fmt"
	"testing"

	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/conn"
	"gvisor.dev/gvisor/pkg/tcpip"
	gvisorchecksum "gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// The kernel hands us an entire GSO packet whose payload is itself a GSO packet
// encapsulated by a UDP tunnel -- VxLAN and friends -- once the UDP tunnel
// offloads are enabled. The tests below build such a packet, the way the kernel
// does, and check that splitting it into MTU sized packets produces packets
// whose every header field adds up.

const (
	vxlanPort = 4789
	testVNI   = 42

	outerIPID = 7
	innerIPID = 400
	innerSeq  = 1000
)

// tunnelGSOSpec describes the encapsulated GSO packet that build produces.
type tunnelGSOSpec struct {
	outerV6   bool // the tunnel's IP header is IPv6
	innerV6   bool // the encapsulated IP header is IPv6
	innerUDP  bool // the encapsulated transport is UDP (USO) rather than TCP
	outerCSum bool // the tunnel's UDP checksum is offloaded to us
	segs      int
	gsoSize   int
	tailLen   int // payload size of the last segment, if smaller than gsoSize
}

// tunnelGSOOffsets records where each header of an encapsulated GSO packet sits.
type tunnelGSOOffsets struct {
	outerUdpAt  int
	innerNhAt   int
	transportAt int
	hdrLen      int
}

func (s tunnelGSOSpec) outerIphLen() int {
	if s.outerV6 {
		return ipv6HeaderLen
	}
	return ipv4HeaderLen
}

func (s tunnelGSOSpec) innerIphLen() int {
	if s.innerV6 {
		return ipv6HeaderLen
	}
	return ipv4HeaderLen
}

func (s tunnelGSOSpec) transportLen() int {
	if s.innerUDP {
		return udphLen
	}
	return 20
}

func (s tunnelGSOSpec) offsets() tunnelGSOOffsets {
	o := tunnelGSOOffsets{
		outerUdpAt:  s.outerIphLen(),
		innerNhAt:   s.outerIphLen() + udphLen + vxlanLen,
		transportAt: s.outerIphLen() + udphLen + vxlanLen + s.innerIphLen(),
	}
	o.hdrLen = o.transportAt + s.transportLen()
	return o
}

func (s tunnelGSOSpec) segDataLen(i int) int {
	if i == s.segs-1 && s.tailLen != 0 {
		return s.tailLen
	}
	return s.gsoSize
}

func (s tunnelGSOSpec) payloadLen() int {
	total := 0
	for i := range s.segs {
		total += s.segDataLen(i)
	}
	return total
}

func (s tunnelGSOSpec) addrs(outer bool) (src, dst tcpip.Address) {
	if outer {
		if s.outerV6 {
			return tcpip.AddrFromSlice(ip6PortA.Addr().AsSlice()), tcpip.AddrFromSlice(ip6PortB.Addr().AsSlice())
		}
		return tcpip.AddrFromSlice(ip4PortA.Addr().AsSlice()), tcpip.AddrFromSlice(ip4PortB.Addr().AsSlice())
	}
	if s.innerV6 {
		return tcpip.AddrFromSlice(ip6PortC.Addr().AsSlice()), tcpip.AddrFromSlice(ip6PortA.Addr().AsSlice())
	}
	return tcpip.AddrFromSlice(ip4PortC.Addr().AsSlice()), tcpip.AddrFromSlice(ip4PortA.Addr().AsSlice())
}

// build returns an encapsulated GSO packet prefixed by the extended virtio net
// header that describes it, as the kernel hands them to us.
func (s tunnelGSOSpec) build() []byte {
	o := s.offsets()
	total := o.hdrLen + s.payloadLen()
	b := make([]byte, virtioNetHdrTnlLen+total, 65535)
	pkt := b[virtioNetHdrTnlLen:]

	// The encapsulating IP header.
	outerSrc, outerDst := s.addrs(true)
	if s.outerV6 {
		ipv6H := header.IPv6(pkt)
		ipv6H.Encode(&header.IPv6Fields{
			SrcAddr:           outerSrc,
			DstAddr:           outerDst,
			TransportProtocol: unix.IPPROTO_UDP,
			HopLimit:          64,
			PayloadLength:     uint16(total - ipv6HeaderLen),
		})
	} else {
		ipv4H := header.IPv4(pkt)
		ipv4H.Encode(&header.IPv4Fields{
			SrcAddr:     outerSrc,
			DstAddr:     outerDst,
			Protocol:    unix.IPPROTO_UDP,
			TTL:         64,
			ID:          outerIPID,
			TotalLength: uint16(total),
		})
		ipv4H.SetChecksum(^ipv4H.CalculateChecksum())
	}

	// The encapsulating UDP header and VxLAN header.
	udpH := header.UDP(pkt[o.outerUdpAt:])
	udpH.Encode(&header.UDPFields{
		SrcPort: vxlanPort,
		DstPort: vxlanPort,
		Length:  uint16(total - o.outerUdpAt),
	})
	pkt[o.outerUdpAt+udphLen] = 0x08 // VxLAN's "I" flag, marking a valid VNI
	binary.BigEndian.PutUint32(pkt[o.outerUdpAt+udphLen+4:], testVNI<<8)
	if s.outerCSum {
		// The kernel leaves the checksum of the entire GSO packet where the
		// checksum of every segment will have to go.
		psum := header.PseudoHeaderChecksum(unix.IPPROTO_UDP, outerSrc, outerDst, uint16(total-o.outerUdpAt))
		udpH.SetChecksum(^gvisorchecksum.Checksum(pkt[o.outerUdpAt:], psum))
	}

	// The encapsulated IP header.
	innerSrc, innerDst := s.addrs(false)
	if s.innerV6 {
		ipv6H := header.IPv6(pkt[o.innerNhAt:])
		ipv6H.Encode(&header.IPv6Fields{
			SrcAddr:           innerSrc,
			DstAddr:           innerDst,
			TransportProtocol: s.protocol(),
			HopLimit:          64,
			PayloadLength:     uint16(total - o.innerNhAt - ipv6HeaderLen),
		})
	} else {
		ipv4H := header.IPv4(pkt[o.innerNhAt:])
		ipv4H.Encode(&header.IPv4Fields{
			SrcAddr:     innerSrc,
			DstAddr:     innerDst,
			Protocol:    uint8(s.protocol()),
			TTL:         64,
			ID:          innerIPID,
			TotalLength: uint16(total - o.innerNhAt),
		})
		ipv4H.SetChecksum(^ipv4H.CalculateChecksum())
	}

	// The encapsulated transport header. The kernel hands us a CHECKSUM_PARTIAL
	// packet, in which the checksum field holds the pseudo header checksum,
	// awaiting the rest of the packet.
	psum := header.PseudoHeaderChecksum(s.protocol(), innerSrc, innerDst, uint16(s.transportLen()+s.payloadLen()))
	if s.innerUDP {
		udpH := header.UDP(pkt[o.transportAt:])
		udpH.Encode(&header.UDPFields{
			SrcPort: 1,
			DstPort: 2,
			Length:  uint16(s.transportLen() + s.payloadLen()),
		})
		udpH.SetChecksum(^psum)
	} else {
		tcpH := header.TCP(pkt[o.transportAt:])
		tcpH.Encode(&header.TCPFields{
			SrcPort:    1,
			DstPort:    2,
			SeqNum:     innerSeq,
			AckNum:     1,
			DataOffset: 20,
			Flags:      header.TCPFlagAck | header.TCPFlagPsh,
			WindowSize: 43210,
		})
		tcpH.SetChecksum(^psum)
	}

	// The payload of every segment.
	for i := range pkt[o.hdrLen:] {
		pkt[o.hdrLen+i] = byte(i)
	}

	// The extended virtio net header.
	tunnelType := uint8(virtioNetHdrGSOUDPTunnelIPv4)
	if s.outerV6 {
		tunnelType = virtioNetHdrGSOUDPTunnelIPv6
	}
	gsoType := tunnelType
	switch {
	case s.innerUDP:
		gsoType |= unix.VIRTIO_NET_HDR_GSO_UDP_L4
	case s.innerV6:
		gsoType |= unix.VIRTIO_NET_HDR_GSO_TCPV6
	default:
		gsoType |= unix.VIRTIO_NET_HDR_GSO_TCPV4
	}
	flags := uint8(unix.VIRTIO_NET_HDR_F_NEEDS_CSUM)
	if s.outerCSum {
		flags |= virtioNetHdrFUDPTunnelCSum
	}
	csumOffset := uint16(16)
	if s.innerUDP {
		csumOffset = 6
	}
	hdr := virtioNetHdrTnl{
		virtioNetHdr: virtioNetHdr{
			flags:      flags,
			gsoType:    gsoType,
			hdrLen:     uint16(o.hdrLen),
			gsoSize:    uint16(s.gsoSize),
			csumStart:  uint16(o.transportAt),
			csumOffset: csumOffset,
		},
		outerThOffset: uint16(o.outerUdpAt),
		innerNhOffset: uint16(o.innerNhAt),
	}
	if err := hdr.encode(b); err != nil {
		panic(err)
	}
	return b
}

func (s tunnelGSOSpec) protocol() tcpip.TransportProtocolNumber {
	if s.innerUDP {
		return unix.IPPROTO_UDP
	}
	return unix.IPPROTO_TCP
}

// validate checks that seg is the i'th segment of the packet that build
// produced, down to every length, identification, sequence number, and checksum.
func (s tunnelGSOSpec) validate(t *testing.T, seg []byte, i int) {
	t.Helper()
	o := s.offsets()
	dataLen := s.segDataLen(i)
	if len(seg) != o.hdrLen+dataLen {
		t.Fatalf("segment %d: got length %d, want %d", i, len(seg), o.hdrLen+dataLen)
	}

	// The encapsulating IP header covers the entire segment.
	outerSrc, outerDst := s.addrs(true)
	if s.outerV6 {
		ipv6H := header.IPv6(seg)
		if int(ipv6H.PayloadLength()) != len(seg)-ipv6HeaderLen {
			t.Errorf("segment %d: outer IPv6 payload length is %d, want %d", i, ipv6H.PayloadLength(), len(seg)-ipv6HeaderLen)
		}
	} else {
		ipv4H := header.IPv4(seg)
		if int(ipv4H.TotalLength()) != len(seg) {
			t.Errorf("segment %d: outer IPv4 total length is %d, want %d", i, ipv4H.TotalLength(), len(seg))
		}
		if ipv4H.ID() != outerIPID+uint16(i) {
			t.Errorf("segment %d: outer IPv4 ID is %d, want %d", i, ipv4H.ID(), outerIPID+uint16(i))
		}
		if gvisorchecksum.Checksum(seg[:ipv4HeaderLen], 0) != 0xffff {
			t.Errorf("segment %d: outer IPv4 header checksum is invalid", i)
		}
	}

	// The encapsulating UDP header covers the encapsulated packet.
	udpH := header.UDP(seg[o.outerUdpAt:])
	udpLen := uint16(len(seg) - o.outerUdpAt)
	if udpH.Length() != udpLen {
		t.Errorf("segment %d: outer UDP length is %d, want %d", i, udpH.Length(), udpLen)
	}
	if s.outerCSum {
		// A correct checksum makes the sum of the pseudo header and the entire
		// UDP header and payload come out as all ones.
		psum := header.PseudoHeaderChecksum(unix.IPPROTO_UDP, outerSrc, outerDst, udpLen)
		if got := gvisorchecksum.Checksum(seg[o.outerUdpAt:], psum); got != 0xffff {
			t.Errorf("segment %d: outer UDP checksum is invalid, sum is %#x", i, got)
		}
		if udpH.Checksum() == 0 {
			t.Errorf("segment %d: outer UDP checksum is zero, which means \"no checksum\"", i)
		}
	} else if udpH.Checksum() != 0 {
		t.Errorf("segment %d: outer UDP checksum is %#x, want it left alone", i, udpH.Checksum())
	}

	// The encapsulated IP header covers the encapsulated payload.
	innerSrc, innerDst := s.addrs(false)
	if s.innerV6 {
		ipv6H := header.IPv6(seg[o.innerNhAt:])
		if int(ipv6H.PayloadLength()) != len(seg)-o.innerNhAt-ipv6HeaderLen {
			t.Errorf("segment %d: inner IPv6 payload length is %d, want %d", i, ipv6H.PayloadLength(), len(seg)-o.innerNhAt-ipv6HeaderLen)
		}
	} else {
		ipv4H := header.IPv4(seg[o.innerNhAt:])
		if int(ipv4H.TotalLength()) != len(seg)-o.innerNhAt {
			t.Errorf("segment %d: inner IPv4 total length is %d, want %d", i, ipv4H.TotalLength(), len(seg)-o.innerNhAt)
		}
		if ipv4H.ID() != innerIPID+uint16(i) {
			t.Errorf("segment %d: inner IPv4 ID is %d, want %d", i, ipv4H.ID(), innerIPID+uint16(i))
		}
		if gvisorchecksum.Checksum(seg[o.innerNhAt:o.innerNhAt+ipv4HeaderLen], 0) != 0xffff {
			t.Errorf("segment %d: inner IPv4 header checksum is invalid", i)
		}
	}

	// The encapsulated transport header.
	psum := header.PseudoHeaderChecksum(s.protocol(), innerSrc, innerDst, uint16(s.transportLen()+dataLen))
	if s.innerUDP {
		innerUDPH := header.UDP(seg[o.transportAt:])
		if innerUDPH.Length() != uint16(s.transportLen()+dataLen) {
			t.Errorf("segment %d: inner UDP length is %d, want %d", i, innerUDPH.Length(), s.transportLen()+dataLen)
		}
		if got := gvisorchecksum.Checksum(seg[o.transportAt:], psum); got != 0xffff {
			t.Errorf("segment %d: inner UDP checksum is invalid, sum is %#x", i, got)
		}
	} else {
		tcpH := header.TCP(seg[o.transportAt:])
		if want := uint32(innerSeq + i*s.gsoSize); tcpH.SequenceNumber() != want {
			t.Errorf("segment %d: inner TCP sequence number is %d, want %d", i, tcpH.SequenceNumber(), want)
		}
		if got := gvisorchecksum.Checksum(seg[o.transportAt:], psum); got != 0xffff {
			t.Errorf("segment %d: inner TCP checksum is invalid, sum is %#x", i, got)
		}
		// PSH belongs to the last segment alone.
		wantFlags := header.TCPFlagAck
		if i == s.segs-1 {
			wantFlags |= header.TCPFlagPsh
		}
		if tcpH.Flags() != wantFlags {
			t.Errorf("segment %d: inner TCP flags are %#x, want %#x", i, tcpH.Flags(), wantFlags)
		}
	}

	// The payload of the segment must be the bytes that belong to it.
	src := s.build()
	if want := src[virtioNetHdrTnlLen+o.hdrLen+i*s.gsoSize:][:dataLen]; string(seg[o.hdrLen:]) != string(want) {
		t.Errorf("segment %d: payload does not match", i)
	}
}

func Test_handleVirtioRead_tunnel(t *testing.T) {
	tests := []struct {
		name string
		spec tunnelGSOSpec
	}{
		{"tcp4-in-vxlan4", tunnelGSOSpec{segs: 3, gsoSize: 100, outerCSum: true}},
		{"tcp6-in-vxlan4", tunnelGSOSpec{segs: 3, gsoSize: 100, innerV6: true, outerCSum: true}},
		{"tcp4-in-vxlan6", tunnelGSOSpec{segs: 3, gsoSize: 100, outerV6: true, outerCSum: true}},
		{"tcp6-in-vxlan6", tunnelGSOSpec{segs: 4, gsoSize: 353, outerV6: true, innerV6: true, outerCSum: true}},
		{"uso4-in-vxlan4", tunnelGSOSpec{segs: 3, gsoSize: 100, innerUDP: true, outerCSum: true}},
		{"uso6-in-vxlan6", tunnelGSOSpec{segs: 2, gsoSize: 1000, innerUDP: true, innerV6: true, outerV6: true, outerCSum: true}},
		{"without-outer-checksum", tunnelGSOSpec{segs: 3, gsoSize: 100}},
		{"short-tail-segment", tunnelGSOSpec{segs: 4, gsoSize: 100, tailLen: 37, outerCSum: true}},
		{"single-segment", tunnelGSOSpec{segs: 1, gsoSize: 100, outerCSum: true}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := tt.spec.build()
			out := make([][]byte, conn.IdealBatchSize)
			sizes := make([]int, conn.IdealBatchSize)
			for i := range out {
				out[i] = make([]byte, 65535)
			}
			n, err := handleVirtioRead(in, out, sizes, offset, virtioNetHdrTnlLen)
			if err != nil {
				t.Fatalf("got err: %v", err)
			}
			if n != tt.spec.segs {
				t.Fatalf("got %d packets, want %d", n, tt.spec.segs)
			}
			for i := range n {
				tt.spec.validate(t, out[i][offset:offset+sizes[i]], i)
			}
		})
	}
}

func Test_handleVirtioRead_tunnel_malformed(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(b []byte, s tunnelGSOSpec)
	}{
		{"both-tunnel-versions", func(b []byte, s tunnelGSOSpec) {
			b[1] |= virtioNetHdrGSOUDPTunnel
		}},
		{"no-needs-csum", func(b []byte, s tunnelGSOSpec) {
			b[0] &^= unix.VIRTIO_NET_HDR_F_NEEDS_CSUM
		}},
		{"ufo-inside-tunnel", func(b []byte, s tunnelGSOSpec) {
			b[1] = (b[1] &^ 0xf) | unix.VIRTIO_NET_HDR_GSO_UDP
		}},
		{"outer-th-offset-out-of-bounds", func(b []byte, s tunnelGSOSpec) {
			binary.LittleEndian.PutUint16(b[outerThOffsetAt:], uint16(len(b)))
		}},
		{"inner-nh-offset-out-of-bounds", func(b []byte, s tunnelGSOSpec) {
			binary.LittleEndian.PutUint16(b[innerNhOffsetAt:], uint16(len(b)))
		}},
		{"inner-nh-offset-before-tunnel", func(b []byte, s tunnelGSOSpec) {
			binary.LittleEndian.PutUint16(b[innerNhOffsetAt:], uint16(s.offsets().outerUdpAt))
		}},
		{"csum-start-inside-inner-iph", func(b []byte, s tunnelGSOSpec) {
			binary.LittleEndian.PutUint16(b[6:], uint16(s.offsets().innerNhAt+2))
		}},
		{"csum-start-out-of-bounds", func(b []byte, s tunnelGSOSpec) {
			binary.LittleEndian.PutUint16(b[6:], uint16(len(b)))
		}},
		{"tcp-hlen-too-small", func(b []byte, s tunnelGSOSpec) {
			b[virtioNetHdrTnlLen+s.offsets().transportAt+12] = 4 << 4
		}},
		{"outer-version-mismatch", func(b []byte, s tunnelGSOSpec) {
			b[virtioNetHdrTnlLen] = 6 << 4
		}},
		{"inner-version-mismatch", func(b []byte, s tunnelGSOSpec) {
			b[virtioNetHdrTnlLen+s.offsets().innerNhAt] = 6 << 4
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := tunnelGSOSpec{segs: 3, gsoSize: 100, outerCSum: true}
			in := spec.build()
			tt.mutate(in, spec)
			out := make([][]byte, conn.IdealBatchSize)
			sizes := make([]int, conn.IdealBatchSize)
			for i := range out {
				out[i] = make([]byte, 65535)
			}
			if n, err := handleVirtioRead(in, out, sizes, offset, virtioNetHdrTnlLen); err == nil {
				t.Fatalf("expected an error, got %d packets", n)
			} else {
				t.Logf("got expected error: %v", err)
			}
		})
	}
}

// Test_handleVirtioRead_tunnel_notEnoughBuffers checks that a GSO packet with
// more segments than we have buffers for is reported as such, rather than
// writing past the end of bufs.
func Test_handleVirtioRead_tunnel_notEnoughBuffers(t *testing.T) {
	spec := tunnelGSOSpec{segs: conn.IdealBatchSize + 5, gsoSize: 100, outerCSum: true}
	in := spec.build()
	out := make([][]byte, conn.IdealBatchSize)
	sizes := make([]int, conn.IdealBatchSize)
	for i := range out {
		out[i] = make([]byte, 65535)
	}
	n, err := handleVirtioRead(in, out, sizes, offset, virtioNetHdrTnlLen)
	if err != ErrTooManySegments {
		t.Fatalf("got err %v, want ErrTooManySegments", err)
	}
	// gsoSplit reports the number of complete buffers it filled before it ran
	// out of them.
	if want := conn.IdealBatchSize - 1; n != want {
		t.Fatalf("got %d packets, want %d", n, want)
	}
}

// Test_handleVirtioRead_tunnel_matchesPlain checks that encapsulating a packet
// changes nothing but its headers: the segments we produce for a plain GSO
// packet and for the very same packet inside a tunnel carry the same payload
// and the same inner headers.
func Test_handleVirtioRead_tunnel_matchesPlain(t *testing.T) {
	spec := tunnelGSOSpec{segs: 3, gsoSize: 100, outerCSum: true}
	in := spec.build()
	out := make([][]byte, conn.IdealBatchSize)
	sizes := make([]int, conn.IdealBatchSize)
	for i := range out {
		out[i] = make([]byte, 65535)
	}
	n, err := handleVirtioRead(in, out, sizes, offset, virtioNetHdrTnlLen)
	if err != nil {
		t.Fatalf("got err: %v", err)
	}
	o := spec.offsets()
	for i := range n {
		seg := out[i][offset : offset+sizes[i]]
		inner := seg[o.innerNhAt:]
		// The inner packet must be self-consistent: its own IP header must
		// describe exactly what is left of the segment.
		ipv4H := header.IPv4(inner)
		if int(ipv4H.TotalLength()) != len(inner) {
			t.Errorf("segment %d: inner IPv4 total length is %d, want %d", i, ipv4H.TotalLength(), len(inner))
		}
		if fmt.Sprint(ipv4H.SourceAddress()) != "192.0.2.3" {
			t.Errorf("segment %d: inner source address is %v", i, ipv4H.SourceAddress())
		}
	}
}
