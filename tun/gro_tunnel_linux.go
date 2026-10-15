/* SPDX-License-Identifier: MIT */

package tun

import (
	"encoding/binary"

	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/conn"
)

/* Ingress tunnel GRO.
 *
 * A packet that arrives off the wire encapsulated by a UDP tunnel is, from the
 * point of view of the plain GRO tables, a single UDP datagram: the outer UDP
 * header bounds the coalescing, so a batch of N encapsulated segments is
 * written to the TUN device as one GSO_UDP_L4 skb whose segments are the outer
 * datagrams. The kernel then decapsulates each datagram separately, producing N
 * inner skbs, and the inner stack has to reassemble them.
 *
 * The tunnel GSO format the kernel accepts on write describes the *inner*
 * packet instead, so N encapsulated segments become one write, one outer GSO
 * skb, and one inner GSO skb after vxlan_rcv() has stripped the encapsulation.
 * This file recognises the encapsulated packets, coalesces them, and emits the
 * header that describes the result.
 *
 * The inner transport may be TCP or UDP. TCP segments are merged on their inner
 * sequence numbers, the way the kernel merges them in tcp_gro_receive(). UDP
 * datagrams carry no sequence numbers, so they are merged on the weaker rule
 * udp_gro_receive() uses: same inner flow, identical segment size, and nothing
 * smaller already at the tail. The encoding emitted for each is the one the
 * kernel itself produces in udp4_gro_complete() and tcp_gro_complete().
 */

const (
	ethHdrLen   = 14
	ethTypeIPv4 = 0x0800
	ethTypeIPv6 = 0x86dd

	vxlanFlagI         = 0x08      // the only VxLAN flag we accept; GPE sets others
	maxTnlCoalescedLen = maxUint16 // the kernel's GSO size limit
)

// tunTunnelOuterThBias pads the outer transport offset we hand the kernel on
// write. virtio_net_hdr_tnl_to_skb() validates outer_th_offset against
// ETH_HLEN + the outer IP header, because a TAP device delivers an Ethernet
// header first. IFF_TUN does not, so the offset we mean is always below the
// bound the kernel enforces. ip_rcv_core() resets the transport header from the
// IP header's IHL field before anything reads it, so the padded value only has
// to satisfy the check.
const tunTunnelOuterThBias = ethHdrLen

// vxlanUDPPorts are the UDP destination ports we will look for an encapsulated
// packet on. The kernel binds its VxLAN sockets to a configured port and we
// have no view of that configuration, so we recognise the registered defaults.
// A tunnel running on some other port is still forwarded, just coalesced as
// plain UDP.
var vxlanUDPPorts = [...]uint16{4789, 8472, 4790}

func isVxlanPort(port uint16) bool {
	for _, p := range vxlanUDPPorts {
		if p == port {
			return true
		}
	}
	return false
}

// parseTunnel parses a packet encapsulated by a UDP tunnel and returns the
// layout of its headers along with the tunnel's VNI. Only the encapsulation
// that the kernel's ingress tunnel GSO path understands is accepted: a plain,
// non-GPE VxLAN header carrying an untagged IPv4 or IPv6 packet whose transport
// is TCP or UDP. Anything else reports false and is left to the plain GRO path.
func parseTunnel(b []byte) (gsoLayout, uint32, bool) {
	var l gsoLayout
	if len(b) < ipv4HeaderLen {
		return l, 0, false
	}
	switch b[0] >> 4 {
	case 4:
		if b[0]&0x0F != 5 || b[9] != unix.IPPROTO_UDP {
			return l, 0, false
		}
		l.outerIphLen = ipv4HeaderLen
	case 6:
		if b[6] != unix.IPPROTO_UDP {
			return l, 0, false
		}
		l.outerIphLen = ipv6HeaderLen
		l.outerIsV6 = true
	default:
		return l, 0, false
	}
	l.outerIphAt = 0
	l.outerUdpAt = l.outerIphLen
	// Up to the inner IP header plus the smallest transport header we accept.
	// The transport header itself is bounded exactly once its protocol is known.
	if len(b) < l.outerUdpAt+udphLen+vxlanLen+ethHdrLen+ipv4HeaderLen+udphLen {
		return l, 0, false
	}
	if !isVxlanPort(binary.BigEndian.Uint16(b[l.outerUdpAt+2:])) {
		return l, 0, false
	}
	l.vxlanAt = l.outerUdpAt + udphLen
	if b[l.vxlanAt]&^vxlanFlagI != 0 {
		// Not plain VxLAN: the GPE flag, or a reserved bit that is set, means
		// the encapsulated frame is not an Ethernet frame.
		return l, 0, false
	}
	vni := binary.BigEndian.Uint32(b[l.vxlanAt+4:]) >> 8
	l.innerEthAt = l.vxlanAt + vxlanLen
	innerEthType := binary.BigEndian.Uint16(b[l.innerEthAt+12:])
	l.innerIsV6 = innerEthType == ethTypeIPv6
	if !l.innerIsV6 && innerEthType != ethTypeIPv4 {
		// ARP and tagged frames are not something we can describe.
		return l, 0, false
	}
	l.innerIphAt = l.innerEthAt + ethHdrLen
	var innerProto uint8
	if l.innerIsV6 && l.innerIphAt+ipv6HeaderLen > len(b) {
		return l, 0, false
	}
	if l.innerIsV6 {
		if b[l.innerIphAt]>>4 != 6 {
			return l, 0, false
		}
		innerProto = b[l.innerIphAt+6]
		l.innerIphLen = ipv6HeaderLen
	} else {
		if b[l.innerIphAt]>>4 != 4 || b[l.innerIphAt]&0x0F != 5 {
			return l, 0, false
		}
		innerProto = b[l.innerIphAt+9]
		l.innerIphLen = ipv4HeaderLen
	}
	if innerProto != unix.IPPROTO_TCP && innerProto != unix.IPPROTO_UDP {
		return l, 0, false
	}
	l.protocol = innerProto
	// Both transports keep their checksum where the kernel's csum_offset
	// expects it: tcp_gro_complete() and udp_gro_complete() both point
	// csum_start at the transport header, at offsetof(udphdr, check) for UDP
	// and offsetof(tcphdr, check) for TCP.
	l.csumOffset = 16
	if innerProto == unix.IPPROTO_UDP {
		l.csumOffset = 6
	}
	l.transportAt = l.innerIphAt + l.innerIphLen
	transportLen, err := transportHeaderLen(b, l.transportAt, l.protocol)
	if err != nil {
		return l, 0, false
	}
	l.transportLen = transportLen
	return l, vni, true
}

// tunnelFlowKey identifies the flow an encapsulated packet belongs to, which is
// the outer flow, the tunnel, and the inner flow together.
type tunnelFlowKey struct {
	outerSrc   [16]byte
	outerDst   [16]byte
	innerSrc   [16]byte
	innerDst   [16]byte
	outerPorts uint32 // source in the high half
	innerPorts uint32 // source in the high half
	vni        uint32
	outerIsV6  bool
	innerIsV6  bool
}

func newTunnelFlowKey(pkt []byte, l gsoLayout, vni uint32) tunnelFlowKey {
	// Every offset in the layout was found by parsing the packet, so the
	// addresses the key is built from are within its bounds.
	key := tunnelFlowKey{
		vni:       vni,
		outerIsV6: l.outerIsV6,
		innerIsV6: l.innerIsV6,
	}
	outerAddrLen := 4
	if l.outerIsV6 {
		outerAddrLen = 16
	}
	innerAddrLen := 4
	if l.innerIsV6 {
		innerAddrLen = 16
	}
	copy(key.outerSrc[:], pkt[l.outerIphAt+ipv4SrcAddrOffset:][:outerAddrLen])
	copy(key.outerDst[:], pkt[l.outerIphAt+ipv4SrcAddrOffset+outerAddrLen:][:outerAddrLen])
	copy(key.innerSrc[:], pkt[l.innerIphAt+ipv4SrcAddrOffset:][:innerAddrLen])
	copy(key.innerDst[:], pkt[l.innerIphAt+ipv4SrcAddrOffset+innerAddrLen:][:innerAddrLen])
	key.outerPorts = uint32(binary.BigEndian.Uint16(pkt[l.outerUdpAt:]))<<16 | uint32(binary.BigEndian.Uint16(pkt[l.outerUdpAt+2:]))
	key.innerPorts = uint32(binary.BigEndian.Uint16(pkt[l.transportAt:]))<<16 | uint32(binary.BigEndian.Uint16(pkt[l.transportAt+2:]))
	return key
}

// tunnelGROItem is the bookkeeping for one encapsulated packet during a GRO
// evaluation across a vector of packets.
type tunnelGROItem struct {
	key       tunnelFlowKey
	bufsIndex uint16    // the index into the original bufs slice
	numMerged uint16    // the number of packets merged into this item
	gsoSize   uint16    // inner payload size
	sentSeq   uint32    // inner TCP sequence number of the first segment; unused for UDP
	pshSet    bool      // unused for UDP, which has no flags
	l         gsoLayout // the header layout of the packet that heads the group
}

// tunnelGROTable holds flow and coalescing information for encapsulated
// packets.
type tunnelGROTable struct {
	itemsByFlow map[tunnelFlowKey][]tunnelGROItem
	itemsPool   [][]tunnelGROItem
	stats       TunnelGROStats
}

// TunnelGROStats counts why encapsulated packets were not coalesced as tunnel
// GSO, so that a fallback to plain UDP GRO is observable rather than silent.
type TunnelGROStats struct {
	Packets      uint64 // encapsulated packets seen
	Parsed       uint64 // of those, understood as VxLAN carrying inner TCP or UDP
	NotTunnel    uint64 // outer UDP on a tunnel port, but not understood
	Merged       uint64 // packets merged into an existing group
	NewGroup     uint64 // packets that started a group
	Seq          uint64 // rejected: inner sequence numbers were not adjacent
	Layout       uint64 // rejected: header lengths differed from the template
	OuterHeaders uint64 // rejected: outer IP headers differed
	TunnelFlags  uint64 // rejected: tunnel header flags differed
	PSH          uint64 // rejected: a PSH flag ended the group
	GsoSize      uint64 // rejected: segment sizes were not mergeable
	InvalidCSum  uint64 // rejected: inner transport checksum did not validate
	Capacity     uint64 // rejected: the group would not fit in its buffer
}

func (s *TunnelGROStats) count(reason tunnelRejectReason) {
	switch reason {
	case tunnelRejectLayout:
		s.Layout++
	case tunnelRejectOuterHeaders:
		s.OuterHeaders++
	case tunnelRejectTunnelFlags:
		s.TunnelFlags++
	case tunnelRejectSeq:
		s.Seq++
	case tunnelRejectPSH:
		s.PSH++
	case tunnelRejectGsoSize:
		s.GsoSize++
	default:
	}
}

func newTunnelGROTable() *tunnelGROTable {
	t := &tunnelGROTable{
		itemsByFlow: make(map[tunnelFlowKey][]tunnelGROItem, conn.IdealBatchSize),
		itemsPool:   make([][]tunnelGROItem, conn.IdealBatchSize),
	}
	for i := range t.itemsPool {
		t.itemsPool[i] = make([]tunnelGROItem, 0, conn.IdealBatchSize)
	}
	return t
}

func (t *tunnelGROTable) newItems() []tunnelGROItem {
	var items []tunnelGROItem
	items, t.itemsPool = t.itemsPool[len(t.itemsPool)-1], t.itemsPool[:len(t.itemsPool)-1]
	return items
}

func (t *tunnelGROTable) reset() {
	for k, items := range t.itemsByFlow {
		items = items[:0]
		t.itemsPool = append(t.itemsPool, items)
		delete(t.itemsByFlow, k)
	}
}

func (t *tunnelGROTable) lookupOrInsert(key tunnelFlowKey, l gsoLayout, bufsIndex int, gsoSize uint16, seq uint32, pshSet bool) ([]tunnelGROItem, bool) {
	items, ok := t.itemsByFlow[key]
	if ok {
		return items, ok
	}
	t.insert(key, l, bufsIndex, gsoSize, seq, pshSet)
	return nil, false
}

func (t *tunnelGROTable) insert(key tunnelFlowKey, l gsoLayout, bufsIndex int, gsoSize uint16, seq uint32, pshSet bool) {
	item := tunnelGROItem{
		key:       key,
		bufsIndex: uint16(bufsIndex),
		gsoSize:   gsoSize,
		sentSeq:   seq,
		pshSet:    pshSet,
		l:         l,
	}
	items, ok := t.itemsByFlow[key]
	if !ok {
		items = t.newItems()
	}
	t.stats.NewGroup++
	t.itemsByFlow[key] = append(items, item)
}

func (t *tunnelGROTable) updateAt(item tunnelGROItem, i int) {
	items, _ := t.itemsByFlow[item.key]
	items[i] = item
}

// tunnelRejectReason explains why an encapsulated packet could not join a
// group, so that the reason is countable rather than merely absent.
type tunnelRejectReason uint8

const (
	tunnelRejectNone tunnelRejectReason = iota
	tunnelRejectLayout
	tunnelRejectOuterHeaders
	tunnelRejectTunnelFlags
	tunnelRejectSeq
	tunnelRejectPSH
	tunnelRejectGsoSize
)

// tunnelPacketsCanCoalesce evaluates if pkt can be coalesced with the packet
// described by item, following the rules tcpPacketsCanCoalesce and
// udpPacketsCanCoalesce apply to a plain packet, but against the *inner* transport
// header.
func tunnelPacketsCanCoalesce(pkt []byte, l gsoLayout, gsoSize uint16, seq uint32, pshSet bool, item tunnelGROItem, bufs [][]byte, offset int) (canCoalesce, tunnelRejectReason) {
	pktTarget := bufs[item.bufsIndex][offset:]
	if item.l != l {
		// The kernel describes every segment with the template's header length,
		// so a group may only contain packets whose headers are the same size.
		return coalesceUnavailable, tunnelRejectLayout
	}
	if !ipHeadersCanCoalesce(pkt, pktTarget) {
		return coalesceUnavailable, tunnelRejectOuterHeaders
	}
	if pkt[l.vxlanAt] != pktTarget[item.l.vxlanAt] {
		return coalesceUnavailable, tunnelRejectTunnelFlags
	}
	if l.protocol == unix.IPPROTO_UDP {
		// udp_gro_receive() appends only: a UDP datagram carries no sequence
		// number that would place it relative to the ones already grouped.
		if len(pktTarget[l.transportAt+l.transportLen:])%int(item.gsoSize) != 0 {
			// A smaller than gsoSize packet has been appended previously.
			return coalesceUnavailable, tunnelRejectGsoSize
		}
		if gsoSize > item.gsoSize {
			return coalesceUnavailable, tunnelRejectGsoSize
		}
		return coalesceAppend, tunnelRejectNone
	}
	// seq adjacency, on the inner sequence number
	lhsLen := uint32(item.gsoSize) + uint32(item.numMerged)*uint32(item.gsoSize)
	if seq == item.sentSeq+lhsLen {
		if item.pshSet {
			// We cannot append to a segment that has the PSH flag set.
			return coalesceUnavailable, tunnelRejectPSH
		}
		if len(pktTarget[l.transportAt+l.transportLen:])%int(item.gsoSize) != 0 {
			// A smaller than gsoSize packet has been appended previously.
			return coalesceUnavailable, tunnelRejectGsoSize
		}
		if gsoSize > item.gsoSize {
			return coalesceUnavailable, tunnelRejectGsoSize
		}
		return coalesceAppend, tunnelRejectNone
	} else if seq+uint32(gsoSize) == item.sentSeq {
		if pshSet || item.pshSet {
			// A prepend replaces the header every segment is copied from, so a
			// group whose last segment carries PSH would lose it.
			return coalesceUnavailable, tunnelRejectPSH
		}
		if gsoSize < item.gsoSize {
			return coalesceUnavailable, tunnelRejectGsoSize
		}
		if gsoSize > item.gsoSize && item.numMerged > 0 {
			return coalesceUnavailable, tunnelRejectGsoSize
		}
		return coalescePrepend, tunnelRejectNone
	}
	return coalesceUnavailable, tunnelRejectSeq
}

// innerChecksumIsValid validates the checksum of the encapsulated transport
// header. A zero UDP checksum over IPv4 means "unchecked", which udp_rcv()
// accepts without validating, so it is accepted here too; over IPv6 a UDP
// checksum is mandatory and zero is not.
func innerChecksumIsValid(pkt []byte, l gsoLayout) bool {
	if l.protocol == unix.IPPROTO_UDP && !l.innerIsV6 &&
		binary.BigEndian.Uint16(pkt[l.transportAt+6:]) == 0 {
		return true
	}
	return checksumValidAt(pkt, l.innerIphAt, l.innerIphLen, l.protocol, l.innerIsV6)
}

// coalesceTunnelPackets attempts to coalesce pkt with the packet described by
// item, and returns the outcome. As with coalesceTCPPackets, a prepend swaps
// buf elements, because the index of item is already being tracked for writing.
func coalesceTunnelPackets(mode canCoalesce, pkt []byte, pktBuffsIndex int, gsoSize uint16, seq uint32, pshSet bool, item *tunnelGROItem, bufs [][]byte, offset int) coalesceResult {
	l := item.l
	headersLen := l.transportAt + l.transportLen
	coalescedLen := len(bufs[item.bufsIndex][offset:]) + len(pkt) - headersLen
	if coalescedLen > maxTnlCoalescedLen {
		// The kernel describes the coalesced packet with 16 bit lengths.
		return coalesceInsufficientCap
	}
	// The inner transport checksum of every segment has to be valid, since the
	// kernel hands the coalesced inner packet to the socket as one unit. UDP was
	// already validated in tunnelGRO, which is where it can be tested without
	// first paying for a merge attempt.
	if l.protocol != unix.IPPROTO_UDP {
		if item.numMerged == 0 {
			if !innerChecksumIsValid(bufs[item.bufsIndex][offset:], item.l) {
				return coalesceItemInvalidCSum
			}
		}
		if !innerChecksumIsValid(pkt, l) {
			return coalescePktInvalidCSum
		}
	}
	if mode == coalescePrepend {
		if cap(pkt)-offset < coalescedLen {
			return coalesceInsufficientCap
		}
		if pshSet {
			return coalescePSHEnding
		}
		item.sentSeq = seq
		extendBy := coalescedLen - len(pkt)
		bufs[pktBuffsIndex] = append(bufs[pktBuffsIndex], make([]byte, extendBy)...)
		copy(bufs[pktBuffsIndex][offset+len(pkt):], bufs[item.bufsIndex][offset+headersLen:])
		bufs[item.bufsIndex], bufs[pktBuffsIndex] = bufs[pktBuffsIndex], bufs[item.bufsIndex]
	} else {
		pktHead := bufs[item.bufsIndex][offset:]
		if cap(pktHead)-offset < coalescedLen {
			return coalesceInsufficientCap
		}
		if pshSet {
			item.pshSet = pshSet
			pktHead[l.transportAt+tcpFlagsOffset] |= tcpFlagPSH
		}
		extendBy := len(pkt) - headersLen
		bufs[item.bufsIndex] = append(bufs[item.bufsIndex], make([]byte, extendBy)...)
		copy(bufs[item.bufsIndex][offset+len(pktHead):], pkt[headersLen:])
	}
	if gsoSize > item.gsoSize {
		item.gsoSize = gsoSize
	}
	item.numMerged++
	return coalesceSuccess
}

// tunnelGRO evaluates pkt as an encapsulated packet, coalescing it with an
// existing group or starting a new one. groResultNoop means the packet is not
// one we can describe as tunnel GSO, and the caller should offer it to the
// plain UDP path instead.
func tunnelGRO(bufs [][]byte, offset, pktI int, table *tunnelGROTable) groResult {
	pkt := bufs[pktI][offset:]
	table.stats.Packets++
	l, vni, ok := parseTunnel(pkt)
	if !ok {
		table.stats.NotTunnel++
		return groResultNoop
	}
	if len(pkt) > maxUint16 {
		// A valid IP packet will never exceed this.
		return groResultNoop
	}
	// The packet has to be a complete, unfragmented datagram, both outside and
	// in, in the same way udpGRO and tcpGRO require of a plain packet.
	if !packetLengthsAreConsistent(pkt, l.outerIphAt, l.outerIphLen, l.outerIsV6) ||
		!packetLengthsAreConsistent(pkt, l.innerIphAt, l.innerIphLen, l.innerIsV6) {
		return groResultNoop
	}
	gsoSize := uint16(len(pkt) - l.transportAt - l.transportLen)
	if gsoSize < 1 {
		return groResultNoop
	}
	// UDP offers no sequence numbers to test first, so the checksum is the
	// earliest meaningful condition, and it is the one that decides whether we
	// can vouch for the group at all. Refuse the packet here rather than mid
	// merge, so that an encapsulated datagram we cannot vouch for still reaches
	// the plain UDP path and is coalesced as opaque payload.
	if l.protocol == unix.IPPROTO_UDP && !innerChecksumIsValid(pkt, l) {
		table.stats.NotTunnel++
		return groResultNoop
	}
	table.stats.Parsed++
	// A UDP header is only 8 bytes, so reading a TCP sequence number and flags
	// out of it would run past the header into the payload.
	var seq uint32
	var pshSet bool
	if l.protocol == unix.IPPROTO_TCP {
		seq = binary.BigEndian.Uint32(pkt[l.transportAt+4:])
		pshSet = pkt[l.transportAt+tcpFlagsOffset]&tcpFlagPSH != 0
	}
	key := newTunnelFlowKey(pkt, l, vni)
	items, existing := table.lookupOrInsert(key, l, pktI, gsoSize, seq, pshSet)
	if !existing {
		return groResultTableInsert
	}
	for i := range items {
		item := items[i]
		can, reason := tunnelPacketsCanCoalesce(pkt, l, gsoSize, seq, pshSet, item, bufs, offset)
		if can == coalesceUnavailable {
			table.stats.count(reason)
			continue
		}
		result := coalesceTunnelPackets(can, pkt, pktI, gsoSize, seq, pshSet, &item, bufs, offset)
		switch result {
		case coalesceSuccess:
			table.stats.Merged++
			table.updateAt(item, i)
			return groResultCoalesced
		case coalesceInsufficientCap:
			table.stats.Capacity++
		case coalesceItemInvalidCSum, coalescePktInvalidCSum:
			table.stats.InvalidCSum++
		case coalescePSHEnding:
			table.stats.count(tunnelRejectPSH)
		default:
		}
	}
	table.insert(key, l, pktI, gsoSize, seq, pshSet)
	return groResultTableInsert
}

// packetLengthsAreConsistent reports whether the IP header at iphAt describes a
// packet that ends exactly at the end of pkt, and is not fragmented. iphAt is
// relative to pkt.
func packetLengthsAreConsistent(pkt []byte, iphAt, iphLen int, isV6 bool) bool {
	if iphAt+iphLen > len(pkt) {
		return false
	}
	if isV6 {
		hdr := pkt[iphAt:]
		if hdr[0]>>4 != 6 {
			return false
		}
		return int(binary.BigEndian.Uint16(hdr[4:])) == len(pkt)-iphAt-iphLen
	}
	hdr := pkt[iphAt:]
	if hdr[0]>>4 != 4 {
		return false
	}
	if hdr[6]&ipv4FlagMoreFragments != 0 || hdr[6]<<3 != 0 || hdr[7] != 0 {
		// no GRO support for fragmented segments for now
		return false
	}
	return int(binary.BigEndian.Uint16(hdr[2:])) == len(pkt)-iphAt
}

// applyTunnelCoalesceAccounting rewrites the packet that heads each coalesced
// group so that it describes the whole group, and stores the tunnel GSO header
// that tells the kernel how to read it.
func applyTunnelCoalesceAccounting(bufs [][]byte, offset int, table *tunnelGROTable, store *groHdrStore) error {
	if table == nil {
		return nil
	}
	for _, items := range table.itemsByFlow {
		for _, item := range items {
			if item.numMerged == 0 {
				continue
			}
			pkt := bufs[item.bufsIndex][offset:]
			l := item.l
			tunnelType := uint8(virtioNetHdrGSOUDPTunnelIPv4)
			gsoType := uint8(unix.VIRTIO_NET_HDR_GSO_TCPV4)
			if l.innerIsV6 {
				gsoType = unix.VIRTIO_NET_HDR_GSO_TCPV6
			}
			if l.protocol == unix.IPPROTO_UDP {
				// udp_gro_complete() marks the result GSO_UDP_L4 whatever
				// the inner address family, and virtio_net_hdr_tnl_to_skb()
				// then reads the inner version from inner_nh_offset rather
				// than from the gso type, since for USO the type cannot say.
				gsoType = unix.VIRTIO_NET_HDR_GSO_UDP_L4
			}
			if l.outerIsV6 {
				tunnelType = virtioNetHdrGSOUDPTunnelIPv6
			}
			hdr := virtioNetHdrTnl{
				virtioNetHdr: virtioNetHdr{
					flags:      unix.VIRTIO_NET_HDR_F_NEEDS_CSUM | virtioNetHdrFUDPTunnelCSum,
					hdrLen:     uint16(l.transportAt + l.transportLen),
					gsoSize:    item.gsoSize,
					gsoType:    gsoType | tunnelType,
					csumStart:  uint16(l.transportAt),
					csumOffset: uint16(l.csumOffset),
				},
				// The kernel's bound includes an Ethernet header we do not have.
				outerThOffset: uint16(l.outerUdpAt + tunTunnelOuterThBias),
				innerNhOffset: uint16(l.innerIphAt),
			}
			// The outer and inner IP headers both describe the whole coalesced
			// packet now, and the encapsulating UDP header covers all of it.
			if !l.outerIsV6 {
				pkt[l.outerIphAt+10], pkt[l.outerIphAt+11] = 0, 0
			}
			setIPLen(pkt, l.outerIphAt, l.outerIphLen, len(pkt), 0, l.outerIsV6)
			binary.BigEndian.PutUint16(pkt[l.outerUdpAt+4:], uint16(len(pkt)-l.outerUdpAt))
			// The kernel computes a checksum for each encapsulating datagram it
			// emits, so the template's own value is not carried over.
			pkt[l.outerUdpAt+6], pkt[l.outerUdpAt+7] = 0, 0
			if !l.innerIsV6 {
				pkt[l.innerIphAt+10], pkt[l.innerIphAt+11] = 0, 0
			}
			setIPLen(pkt, l.innerIphAt, l.innerIphLen, len(pkt)-l.innerIphAt, 0, l.innerIsV6)
			if l.protocol == unix.IPPROTO_UDP {
				// udp4_gro_complete() does udp_set_len(uh, skb->len - nhoff),
				// so the template describes the whole coalesced datagram and
				// __skb_udp_tunnel_segment() rewrites the length per segment.
				binary.BigEndian.PutUint16(pkt[l.transportAt+4:], uint16(len(pkt)-l.transportAt))
			}
			// The inner transport checksum becomes a partial checksum over the
			// whole coalesced group, which the kernel completes per segment.
			srcAddrAt := offset + l.innerIphAt + ipv4SrcAddrOffset
			addrLen := 4
			if l.innerIsV6 {
				srcAddrAt = offset + l.innerIphAt + ipv6SrcAddrOffset
				addrLen = 16
			}
			// udp4_gro_complete() preloads ~udp_v4_check() only when the header
			// carried a checksum; an unchecked one stays zero and the kernel
			// completes a full checksum per segment from the partial offset.
			// The decision is taken on the group template, which is what the
			// emitted header describes.
			unchecked := l.protocol == unix.IPPROTO_UDP && !l.innerIsV6 &&
				binary.BigEndian.Uint16(pkt[l.transportAt+l.csumOffset:]) == 0
			if unchecked {
				binary.BigEndian.PutUint16(pkt[l.transportAt+l.csumOffset:], 0)
			} else {
				srcAddr := bufs[item.bufsIndex][srcAddrAt : srcAddrAt+addrLen]
				dstAddr := bufs[item.bufsIndex][srcAddrAt+addrLen : srcAddrAt+addrLen*2]
				psum := pseudoHeaderChecksumNoFold(l.protocol, srcAddr, dstAddr, uint16(len(pkt)-l.innerIphAt-l.innerIphLen))
				binary.BigEndian.PutUint16(pkt[l.transportAt+l.csumOffset:], checksum([]byte{}, psum))
			}
			if err := hdr.encode(store.tnlAt(int(item.bufsIndex))); err != nil {
				return err
			}
		}
	}
	return nil
}
