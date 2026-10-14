/* SPDX-License-Identifier: MIT */

package tun

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strconv"
	"testing"

	"golang.org/x/sys/unix"
)

// tnlFixture builds packets as they arrive off the wire: a TCP segment inside
// an Ethernet frame inside a VxLAN datagram.
type tnlFixture struct {
	outerV6, innerV6       bool
	outerSrc, outerDst     []byte
	innerSrc, innerDst     []byte
	outerSport, outerDport uint16
	innerSport, innerDport uint16
	vni                    uint32
	udpCsum                bool
	outerIPID              uint16
	innerIPID              uint16
	ack                    uint32
	window                 uint16
	tcpOptions             int
}

func newTnlFixture(outerV6, innerV6 bool) *tnlFixture {
	f := &tnlFixture{
		outerV6:    outerV6,
		innerV6:    innerV6,
		outerSrc:   []byte{192, 0, 2, 1},
		outerDst:   []byte{192, 0, 2, 2},
		innerSrc:   []byte{10, 1, 0, 1},
		innerDst:   []byte{10, 1, 0, 2},
		outerSport: 51000,
		outerDport: 4789,
		innerSport: 22,
		innerDport: 44000,
		vni:        0x010203,
		udpCsum:    true,
		ack:        7000,
		window:     65535,
	}
	if outerV6 {
		f.outerSrc = bytes.Repeat([]byte{0x20, 0x01, 0x0d, 0xb8}, 4)
		f.outerDst = bytes.Repeat([]byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2}, 1)
	}
	if innerV6 {
		f.innerSrc = []byte{0xfe, 0x80, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}
		f.innerDst = []byte{0xfe, 0x80, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2}
	}
	return f
}

func (f *tnlFixture) outerIphLen() int {
	if f.outerV6 {
		return ipv6HeaderLen
	}
	return ipv4HeaderLen
}

func (f *tnlFixture) innerIphLen() int {
	if f.innerV6 {
		return ipv6HeaderLen
	}
	return ipv4HeaderLen
}

func (f *tnlFixture) tcpAt() int {
	return f.outerIphLen() + udphLen + vxlanLen + ethHdrLen + f.innerIphLen()
}

// pkt builds one encapsulated TCP segment carrying payload.
func (f *tnlFixture) pkt(seq uint32, payload []byte, flags byte) []byte {
	tcpLen := 20 + f.tcpOptions
	total := f.tcpAt() + tcpLen + len(payload)
	b := make([]byte, total)

	// outer IP header
	if f.outerV6 {
		b[0] = 6 << 4
		binary.BigEndian.PutUint16(b[4:], uint16(total-ipv6HeaderLen))
		b[6] = unix.IPPROTO_UDP
		b[7] = 64
		copy(b[8:], f.outerSrc)
		copy(b[24:], f.outerDst)
	} else {
		b[0] = 4<<4 | 5
		binary.BigEndian.PutUint16(b[2:], uint16(total))
		binary.BigEndian.PutUint16(b[4:], f.outerIPID)
		b[9] = unix.IPPROTO_UDP
		copy(b[12:], f.outerSrc)
		copy(b[16:], f.outerDst)
		binary.BigEndian.PutUint16(b[10:], ^checksum(b[:ipv4HeaderLen], 0))
	}

	// encapsulating UDP header
	udpAt := f.outerIphLen()
	binary.BigEndian.PutUint16(b[udpAt:], f.outerSport)
	binary.BigEndian.PutUint16(b[udpAt+2:], f.outerDport)
	binary.BigEndian.PutUint16(b[udpAt+4:], uint16(total-udpAt))

	// VxLAN header
	vxlanAt := udpAt + udphLen
	b[vxlanAt] = vxlanFlagI
	b[vxlanAt+4] = byte(f.vni >> 16)
	b[vxlanAt+5] = byte(f.vni >> 8)
	b[vxlanAt+6] = byte(f.vni)

	// encapsulated Ethernet header
	ethAt := vxlanAt + vxlanLen
	ethTypeAt := ethAt + 12
	if f.innerV6 {
		binary.BigEndian.PutUint16(b[ethTypeAt:], ethTypeIPv6)
	} else {
		binary.BigEndian.PutUint16(b[ethTypeAt:], ethTypeIPv4)
	}

	// inner IP header
	innerIphAt := ethAt + ethHdrLen
	if f.innerV6 {
		b[innerIphAt] = 6 << 4
		binary.BigEndian.PutUint16(b[innerIphAt+4:], uint16(total-innerIphAt-ipv6HeaderLen))
		b[innerIphAt+6] = unix.IPPROTO_TCP
		b[innerIphAt+7] = 64
		copy(b[innerIphAt+8:], f.innerSrc)
		copy(b[innerIphAt+24:], f.innerDst)
	} else {
		b[innerIphAt] = 4<<4 | 5
		binary.BigEndian.PutUint16(b[innerIphAt+2:], uint16(total-innerIphAt))
		binary.BigEndian.PutUint16(b[innerIphAt+4:], f.innerIPID)
		b[innerIphAt+9] = unix.IPPROTO_TCP
		copy(b[innerIphAt+12:], f.innerSrc)
		copy(b[innerIphAt+16:], f.innerDst)
		binary.BigEndian.PutUint16(b[innerIphAt+10:], ^checksum(b[innerIphAt:innerIphAt+ipv4HeaderLen], 0))
	}

	// inner TCP header
	tcpAt := innerIphAt + f.innerIphLen()
	binary.BigEndian.PutUint16(b[tcpAt:], f.innerSport)
	binary.BigEndian.PutUint16(b[tcpAt+2:], f.innerDport)
	binary.BigEndian.PutUint32(b[tcpAt+4:], seq)
	binary.BigEndian.PutUint32(b[tcpAt+8:], f.ack)
	b[tcpAt+12] = byte(tcpLen / 4 << 4)
	b[tcpAt+13] = flags
	binary.BigEndian.PutUint16(b[tcpAt+14:], f.window)
	if f.tcpOptions > 0 {
		copy(b[tcpAt+20:], bytes.Repeat([]byte{byte(0x02)}, f.tcpOptions-2)) // MSS option
	}
	copy(b[tcpAt+tcpLen:], payload)

	// inner TCP checksum
	psum := pseudoHeaderChecksumNoFold(unix.IPPROTO_TCP, f.innerSrc, f.innerDst, uint16(tcpLen+len(payload)))
	binary.BigEndian.PutUint16(b[tcpAt+16:], ^checksum(b[tcpAt:], psum))

	// encapsulating UDP checksum
	if f.udpCsum {
		psum := pseudoHeaderChecksumNoFold(unix.IPPROTO_UDP, f.outerSrc, f.outerDst, uint16(total-udpAt))
		csum := ^checksum(b[udpAt:], psum)
		if csum == 0 {
			csum = 0xffff
		}
		binary.BigEndian.PutUint16(b[udpAt+6:], csum)
	}
	return b
}

// tnlBufs lays packets out the way the receive path does, with only
// MessageTransportOffsetContent bytes in front of each one.
func tnlBufs(pkts [][]byte) ([][]byte, int) {
	const offset = 16
	bufs := make([][]byte, len(pkts))
	for i, pkt := range pkts {
		buf := make([]byte, offset+len(pkt), offset+65535)
		copy(buf[offset:], pkt)
		bufs[i] = buf
	}
	return bufs, offset
}

func tnlHdrScratch(n int) [][]byte {
	hdrs := make([][]byte, n)
	for i := range hdrs {
		hdrs[i] = make([]byte, virtioNetHdrTnlLen)
	}
	return hdrs
}

// asReadByKernel reports a header slot the way the kernel reports one when it
// hands us a packet. We pad outerThOffset on write, because the bound the
// kernel validates it against counts an Ethernet header that IFF_TUN does not
// deliver, but tun_vnet_hdr_from_skb reports the field unbiased. A header we
// emitted therefore has to lose the pad before the split, which models the
// kernel, is given it.
func asReadByKernel(hdr []byte) []byte {
	out := append([]byte{}, hdr...)
	var h virtioNetHdrTnl
	if err := h.decode(out); err != nil {
		return out
	}
	if h.gsoType&virtioNetHdrGSOUDPTunnel != 0 {
		h.outerThOffset -= tunTunnelOuterThBias
		if err := h.encode(out); err != nil {
			return out
		}
	}
	return out
}

// runTnlGRO hands pkts to handleGRO and returns the indices it chose to write.
func runTnlGRO(t *testing.T, pkts [][]byte) ([][]byte, [][]byte, []int, *tunnelGROTable) {
	t.Helper()
	bufs, offset := tnlBufs(pkts)
	hdrs := tnlHdrScratch(len(bufs))
	table := newTunnelGROTable()
	var toWrite []int
	err := handleGRO(bufs, offset, newTCPGROTable(), newUDPGROTable(), table, true, hdrs, &toWrite)
	if err != nil {
		t.Fatalf("handleGRO: %v", err)
	}
	return bufs, hdrs, toWrite, table
}

// Test_tunnelGRO_roundTrip is the oracle for the whole ingress path: packets
// coalesced by handleGRO are fed back through the split that mirrors the
// kernel's, and must come out byte for byte as the packets that went in.
func Test_tunnelGRO_roundTrip(t *testing.T) {
	for _, outerV6 := range []bool{false, true} {
		for _, innerV6 := range []bool{false, true} {
			for _, segs := range []int{1, 2, 3, 9} {
				for _, payload := range []int{1, 100, 1360} {
					f := newTnlFixture(outerV6, innerV6)
					var pkts [][]byte
					for i := 0; i < segs; i++ {
						flags := byte(tcpFlagACK)
						if i == segs-1 {
							flags |= tcpFlagPSH
						}
						// A sender segments a GSO packet by incrementing the
						// identification field of every header, so that is what
						// the packets we have to reproduce look like.
						f.outerIPID = uint16(i)
						f.innerIPID = uint16(i)
						pkts = append(pkts, f.pkt(1000+uint32(i*payload), bytes.Repeat([]byte{byte(i)}, payload), flags))
					}
					name := map[bool]string{false: "v4", true: "v6"}
					t.Run("outer"+name[outerV6]+"-inner"+name[innerV6]+"-segs"+strconv.Itoa(segs)+"-payload"+strconv.Itoa(payload), func(t *testing.T) {
						bufs, hdrs, toWrite, table := runTnlGRO(t, pkts)
						if len(toWrite) != 1 {
							t.Fatalf("handleGRO wrote %d packets, want 1", len(toWrite))
						}
						const offset = 16
						in := make([]byte, 0, virtioNetHdrTnlLen+len(bufs[toWrite[0]])-offset)
						in = append(in, asReadByKernel(hdrs[toWrite[0]])...)
						in = append(in, bufs[toWrite[0]][offset:]...)

						outBufs := make([][]byte, segs)
						for i := range outBufs {
							outBufs[i] = make([]byte, 65535)
						}
						sizes := make([]int, segs)
						n, err := handleVirtioRead(in, outBufs, sizes, 0, virtioNetHdrTnlLen)
						if err != nil {
							t.Fatalf("handleVirtioRead: %v", err)
						}
						if n != segs {
							t.Fatalf("split into %d packets, want %d", n, segs)
						}
						for i := 0; i < segs; i++ {
							if !bytes.Equal(outBufs[i][:sizes[i]], pkts[i]) {
								t.Fatalf("segment %d differs:\n got %x\nwant %x", i, outBufs[i][:sizes[i]], pkts[i])
							}
						}
						if want := segs - 1; table.stats.Merged != uint64(want) {
							t.Fatalf("Merged = %d, want %d", table.stats.Merged, want)
						}
					})
				}
			}
		}
	}
}

// Test_tunnelGRO_header checks the header that describes a coalesced group.
func Test_tunnelGRO_header(t *testing.T) {
	f := newTnlFixture(false, false)
	var pkts [][]byte
	for i := 0; i < 4; i++ {
		pkts = append(pkts, f.pkt(1000+uint32(i*100), bytes.Repeat([]byte{byte(i)}, 100), tcpFlagACK))
	}
	bufs, hdrs, toWrite, _ := runTnlGRO(t, pkts)
	if len(toWrite) != 1 {
		t.Fatalf("handleGRO wrote %d packets, want 1", len(toWrite))
	}
	var hdr virtioNetHdrTnl
	if err := hdr.decode(hdrs[toWrite[0]]); err != nil {
		t.Fatalf("decode: %v", err)
	}
	l, _, ok := parseTunnel(pkts[0])
	if !ok {
		t.Fatal("parseTunnel rejected a packet it should accept")
	}
	if hdr.gsoType != unix.VIRTIO_NET_HDR_GSO_TCPV4|virtioNetHdrGSOUDPTunnelIPv4 {
		t.Errorf("gsoType = %#x, want %#x", hdr.gsoType, unix.VIRTIO_NET_HDR_GSO_TCPV4|virtioNetHdrGSOUDPTunnelIPv4)
	}
	if hdr.flags != unix.VIRTIO_NET_HDR_F_NEEDS_CSUM|virtioNetHdrFUDPTunnelCSum {
		t.Errorf("flags = %#x", hdr.flags)
	}
	if int(hdr.hdrLen) != l.transportAt+l.transportLen {
		t.Errorf("hdrLen = %d, want %d", hdr.hdrLen, l.transportAt+l.transportLen)
	}
	if int(hdr.csumStart) != l.transportAt || int(hdr.csumOffset) != 16 {
		t.Errorf("csumStart = %d, csumOffset = %d, want %d, 16", hdr.csumStart, hdr.csumOffset, l.transportAt)
	}
	if int(hdr.gsoSize) != 100 {
		t.Errorf("gsoSize = %d, want 100", hdr.gsoSize)
	}
	// The kernel validates outer_th_offset against an Ethernet header that
	// IFF_TUN does not deliver, so we hand it a padded value.
	if int(hdr.outerThOffset) != l.outerUdpAt+tunTunnelOuterThBias {
		t.Errorf("outerThOffset = %d, want %d", hdr.outerThOffset, l.outerUdpAt+tunTunnelOuterThBias)
	}
	if int(hdr.innerNhOffset) != l.innerIphAt {
		t.Errorf("innerNhOffset = %d, want %d", hdr.innerNhOffset, l.innerIphAt)
	}
	// The lengths in the packet have to describe the whole group.
	pkt := bufs[toWrite[0]][16:]
	if got := binary.BigEndian.Uint16(pkt[2:]); int(got) != len(pkt) {
		t.Errorf("outer total len = %d, want %d", got, len(pkt))
	}
	if got := binary.BigEndian.Uint16(pkt[l.outerUdpAt+4:]); int(got) != len(pkt)-l.outerUdpAt {
		t.Errorf("outer udp len = %d, want %d", got, len(pkt)-l.outerUdpAt)
	}
	if got := binary.BigEndian.Uint16(pkt[l.innerIphAt+2:]); int(got) != len(pkt)-l.innerIphAt {
		t.Errorf("inner total len = %d, want %d", got, len(pkt)-l.innerIphAt)
	}
	if got := binary.BigEndian.Uint16(pkt[l.outerUdpAt+6:]); got != 0 {
		t.Errorf("outer udp checksum = %#x, want 0 (the kernel computes one per segment)", got)
	}
	// The header checksums have to be valid for the packet as rewritten.
	if csum := checksum(pkt[:ipv4HeaderLen], 0); csum != 0xffff {
		t.Errorf("outer ipv4 header checksum is invalid")
	}
	if csum := checksum(pkt[l.innerIphAt:l.innerIphAt+ipv4HeaderLen], 0); csum != 0xffff {
		t.Errorf("inner ipv4 header checksum is invalid")
	}
}

// Test_tunnelGRO_fallbacks covers the cases that must not be described as
// tunnel GSO: either they fall back to plain UDP GRO, or they are understood as
// tunnel packets that simply cannot share a group.
func Test_tunnelGRO_fallbacks(t *testing.T) {
	// resealOuterUDP repairs the encapsulating checksum after a mutation
	// changed the bytes it covers, so that a fallback is attributed to the
	// mutation and not to a checksum the test broke on the way.
	resealOuterUDP := func(p []byte) {
		const udpAt = ipv4HeaderLen
		binary.BigEndian.PutUint16(p[udpAt+6:], 0)
		psum := pseudoHeaderChecksumNoFold(unix.IPPROTO_UDP, []byte{192, 0, 2, 1}, []byte{192, 0, 2, 2}, uint16(len(p)-udpAt))
		csum := ^checksum(p[udpAt:], psum)
		if csum == 0 {
			csum = 0xffff
		}
		binary.BigEndian.PutUint16(p[udpAt+6:], csum)
	}
	// resealInnerTCP repairs the encapsulated TCP checksum, which a mutation of
	// the TCP header itself invalidates.
	resealInnerTCP := func(p []byte) {
		l, _, ok := parseTunnel(p)
		if !ok {
			return
		}
		binary.BigEndian.PutUint16(p[l.transportAt+16:], 0)
		psum := pseudoHeaderChecksumNoFold(unix.IPPROTO_TCP, []byte{10, 1, 0, 1}, []byte{10, 1, 0, 2}, uint16(len(p)-l.transportAt))
		binary.BigEndian.PutUint16(p[l.transportAt+16:], ^checksum(p[l.transportAt:], psum))
	}
	mutate := func(p []byte, what string) []byte {
		l, _, ok := parseTunnel(p)
		if !ok {
			t.Fatal("fixture does not parse")
		}
		switch what {
		case "gpe":
			p[l.vxlanAt] = vxlanFlagI | 0x04 // the GPE next-protocol bit
		case "port":
			binary.BigEndian.PutUint16(p[l.outerUdpAt+2:], 9999)
		case "vni":
			p[l.vxlanAt+4] = 0xff
		case "inner-udp":
			p[l.innerIphAt+9] = unix.IPPROTO_UDP
		case "inner-frag":
			p[l.innerIphAt+6] |= ipv4FlagMoreFragments
		case "outer-frag":
			p[l.outerIphAt+6] |= ipv4FlagMoreFragments
		case "ttl":
			p[l.outerIphAt+8] ^= 0xff
		case "bad-csum":
			p[l.transportAt+16] ^= 0xff
		case "gap":
			binary.BigEndian.PutUint32(p[l.transportAt+4:], 999999)
		case "psh":
			p[l.transportAt+13] |= tcpFlagPSH
			resealInnerTCP(p)
		default:
			t.Fatalf("unknown mutation %q", what)
		}
		if what != "bad-csum" {
			resealOuterUDP(p)
		}
		return p
	}
	cases := []struct {
		what   string
		second bool // only the second packet is mutated
		writes int
		tnl    bool // the group is described as tunnel GSO
	}{
		{what: "none", writes: 1, tnl: true},
		{what: "gpe", writes: 1},
		{what: "port", writes: 1},
		{what: "inner-udp", writes: 1},
		{what: "inner-frag", writes: 1},
		{what: "outer-frag", writes: 2},
		{what: "vni", second: true, writes: 2},
		{what: "ttl", second: true, writes: 2},
		{what: "bad-csum", second: true, writes: 2},
		{what: "gap", second: true, writes: 2},
		{what: "psh", second: true, writes: 1, tnl: true},
	}
	for _, tc := range cases {
		t.Run(tc.what, func(t *testing.T) {
			f := newTnlFixture(false, false)
			p0 := f.pkt(1000, bytes.Repeat([]byte{1}, 100), tcpFlagACK)
			p1 := f.pkt(1100, bytes.Repeat([]byte{2}, 100), tcpFlagACK)
			if tc.what != "none" {
				if !tc.second {
					p0 = mutate(p0, tc.what)
				}
				p1 = mutate(p1, tc.what)
			}
			_, hdrs, toWrite, _ := runTnlGRO(t, [][]byte{p0, p1})
			if len(toWrite) != tc.writes {
				t.Fatalf("handleGRO wrote %d packets, want %d", len(toWrite), tc.writes)
			}
			if len(toWrite) != 1 {
				return
			}
			var hdr virtioNetHdrTnl
			if err := hdr.decode(hdrs[toWrite[0]]); err != nil {
				t.Fatalf("decode: %v", err)
			}
			isTnl := hdr.gsoType&virtioNetHdrGSOUDPTunnel != 0
			if isTnl != tc.tnl {
				t.Fatalf("tunnel GSO = %v, want %v (gsoType %#x)", isTnl, tc.tnl, hdr.gsoType)
			}
			if !tc.tnl && hdr.gsoType != unix.VIRTIO_NET_HDR_GSO_UDP_L4 {
				t.Fatalf("fallback gsoType = %#x, want plain UDP GSO %#x", hdr.gsoType, unix.VIRTIO_NET_HDR_GSO_UDP_L4)
			}
		})
	}
}

// Test_tunnelGRO_outOfOrder checks that segments that arrive out of order are
// still coalesced, which requires prepending to a group.
func Test_tunnelGRO_outOfOrder(t *testing.T) {
	f := newTnlFixture(false, false)
	var pkts [][]byte
	for i := 0; i < 4; i++ {
		pkts = append(pkts, f.pkt(1000+uint32(i*100), bytes.Repeat([]byte{byte(i)}, 100), tcpFlagACK))
	}
	// 0, 1, 3, 2: the last two have to be prepended onto each other.
	order := []int{0, 1, 3, 2}
	ordered := make([][]byte, 0, len(order))
	for _, i := range order {
		ordered = append(ordered, pkts[i])
	}
	bufs, hdrs, toWrite, _ := runTnlGRO(t, ordered)
	if len(toWrite) != 2 {
		t.Fatalf("handleGRO wrote %d packets, want 2", len(toWrite))
	}
	const offset = 16
	total := 0
	for _, i := range toWrite {
		in := append(append([]byte{}, asReadByKernel(hdrs[i])...), bufs[i][offset:]...)
		outBufs := [][]byte{make([]byte, 65535), make([]byte, 65535), make([]byte, 65535)}
		sizes := make([]int, 3)
		n, err := handleVirtioRead(in, outBufs, sizes, 0, virtioNetHdrTnlLen)
		if err != nil {
			t.Fatalf("handleVirtioRead: %v", err)
		}
		total += n
	}
	if total != 4 {
		t.Fatalf("split into %d packets in total, want 4", total)
	}
}

// Test_tunnelGRO_stats checks that a fallback always says why.
func Test_tunnelGRO_stats(t *testing.T) {
	f := newTnlFixture(false, false)
	bufs, offset := tnlBufs([][]byte{
		f.pkt(1000, bytes.Repeat([]byte{1}, 100), tcpFlagACK),
		f.pkt(1200, bytes.Repeat([]byte{2}, 100), tcpFlagACK), // a gap of 100 bytes
	})
	hdrs := tnlHdrScratch(len(bufs))
	table := newTunnelGROTable()
	var toWrite []int
	if err := handleGRO(bufs, offset, newTCPGROTable(), newUDPGROTable(), table, true, hdrs, &toWrite); err != nil {
		t.Fatalf("handleGRO: %v", err)
	}
	if len(toWrite) != 2 {
		t.Fatalf("handleGRO wrote %d packets, want 2", len(toWrite))
	}
	stats := table.stats
	if stats.Packets != 2 || stats.Parsed != 2 {
		t.Errorf("stats = %+v", stats)
	}
	if stats.NewGroup != 2 {
		t.Errorf("NewGroup = %d, want 2", stats.NewGroup)
	}
	if stats.Seq != 1 {
		t.Errorf("Seq = %d, want 1", stats.Seq)
	}
}

// Test_tunnelGRO_tcpOptions checks that segments with TCP options coalesce, and
// that a segment whose options differ does not.
func Test_tunnelGRO_tcpOptions(t *testing.T) {
	f := newTnlFixture(false, false)
	f.tcpOptions = 4
	same := f.pkt(1100, bytes.Repeat([]byte{2}, 100), tcpFlagACK)
	if _, _, toWrite, _ := runTnlGRO(t, [][]byte{f.pkt(1000, bytes.Repeat([]byte{1}, 100), tcpFlagACK), same}); len(toWrite) != 1 {
		t.Fatalf("identical options did not coalesce: %d writes", len(toWrite))
	}
	other := *f
	other.tcpOptions = 8
	p := other.pkt(1100, bytes.Repeat([]byte{2}, 100), tcpFlagACK)
	if _, _, toWrite, _ := runTnlGRO(t, [][]byte{f.pkt(1000, bytes.Repeat([]byte{1}, 100), tcpFlagACK), p}); len(toWrite) != 2 {
		t.Fatalf("differing options coalesced: %d writes", len(toWrite))
	}
}

// Fuzz_tunnelGRO drives the ingress tunnel path with packets that start out
// well formed and are then mutated, so that the parser is reached at all, and
// feeds whatever comes out back through the split, which must accept it.
func Fuzz_tunnelGRO(f *testing.F) {
	fx := newTnlFixture(false, false)
	seeds := make([][]byte, 4)
	for i := range seeds {
		seeds[i] = fx.pkt(1000+uint32(i*100), bytes.Repeat([]byte{byte(i)}, 100), tcpFlagACK)
	}
	// Reused across executions: allocating 128 maximum size buffers per
	// execution costs more than the code under test.
	outBufs := make([][]byte, 128)
	for i := range outBufs {
		outBufs[i] = make([]byte, 65535)
	}
	sizes := make([]int, len(outBufs))
	f.Add(seeds[0], seeds[1], seeds[2], seeds[3], uint(0))
	f.Fuzz(func(t *testing.T, p0, p1, p2, p3 []byte, which uint) {
		pkts := [][]byte{p0, p1, p2, p3}
		for _, p := range pkts {
			if len(p) == 0 {
				// handleGRO requires a non-empty packet at its offset, which
				// the receive path always provides.
				return
			}
		}
		if which%2 == 1 {
			fx6 := newTnlFixture(true, true)
			pkts[0] = fx6.pkt(1000, []byte{1}, tcpFlagACK)
		}
		bufs, offset := tnlBufs(pkts)
		hdrs := tnlHdrScratch(len(bufs))
		var toWrite []int
		if err := handleGRO(bufs, offset, newTCPGROTable(), newUDPGROTable(), newTunnelGROTable(), true, hdrs, &toWrite); err != nil {
			t.Fatalf("handleGRO: %v", err)
		}
		seen := make(map[int]bool)
		for _, i := range toWrite {
			if i < 0 || i >= len(pkts) {
				t.Fatalf("toWrite index %d outside %d packets", i, len(pkts))
			}
			if seen[i] {
				t.Fatalf("duplicate toWrite index %d", i)
			}
			seen[i] = true
			in := append(append([]byte{}, asReadByKernel(hdrs[i])...), bufs[i][offset:]...)
			n, err := handleVirtioRead(in, outBufs, sizes, 0, virtioNetHdrTnlLen)
			if err != nil && !errors.Is(err, ErrTooManySegments) {
				t.Fatalf("handleVirtioRead rejected what handleGRO produced: %v\nhdr %x\npkt %x", err, hdrs[i], bufs[i][offset:])
			}
			// Every segment is the template's headers plus a share of its
			// payload, so the segments together are the packet plus a copy of
			// the headers for each segment after the first.
			var hdr virtioNetHdrTnl
			if err := hdr.decode(hdrs[i]); err != nil {
				t.Fatalf("decode: %v", err)
			}
			total := 0
			for j := 0; j < n; j++ {
				total += sizes[j]
			}
			if want := len(bufs[i][offset:]) + (n-1)*int(hdr.hdrLen); n > 0 && total != want {
				t.Fatalf("segments total %d, want %d for a %d byte packet with %d byte headers", total, want, len(bufs[i][offset:]), hdr.hdrLen)
			}
		}
	})
}

// Test_tunnelGRO_pshPrepend checks that a segment arriving in front of a group
// whose last segment has PSH does not silently drop the flag, because a prepend
// replaces the header every segment is copied from.
func Test_tunnelGRO_pshPrepend(t *testing.T) {
	f := newTnlFixture(false, false)
	tail := f.pkt(1000, bytes.Repeat([]byte{1}, 100), tcpFlagACK)
	head := f.pkt(1100, bytes.Repeat([]byte{2}, 100), tcpFlagACK|tcpFlagPSH)
	if _, _, toWrite, _ := runTnlGRO(t, [][]byte{head, tail}); len(toWrite) != 2 {
		t.Fatalf("prepend onto a PSH segment merged anyway: %d writes", len(toWrite))
	}
	// Without the flag the same pair is a single group.
	head = f.pkt(1100, bytes.Repeat([]byte{2}, 100), tcpFlagACK)
	if _, _, toWrite, _ := runTnlGRO(t, [][]byte{head, tail}); len(toWrite) != 1 {
		t.Fatalf("prepend without PSH did not merge: %d writes", len(toWrite))
	}
}
