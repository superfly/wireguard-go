/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2017-2025 WireGuard LLC. All Rights Reserved.
 */

package tun

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"unsafe"

	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/conn"
)

const tcpFlagsOffset = 13

const (
	tcpFlagFIN uint8 = 0x01
	tcpFlagPSH uint8 = 0x08
	tcpFlagACK uint8 = 0x10
)

// virtioNetHdr is defined in the kernel in include/uapi/linux/virtio_net.h. The
// kernel symbol is virtio_net_hdr.
type virtioNetHdr struct {
	flags      uint8
	gsoType    uint8
	hdrLen     uint16
	gsoSize    uint16
	csumStart  uint16
	csumOffset uint16
}

func (v *virtioNetHdr) decode(b []byte) error {
	if len(b) < virtioNetHdrLen {
		return io.ErrShortBuffer
	}
	copy(unsafe.Slice((*byte)(unsafe.Pointer(v)), virtioNetHdrLen), b[:virtioNetHdrLen])
	return nil
}

func (v *virtioNetHdr) encode(b []byte) error {
	if len(b) < virtioNetHdrLen {
		return io.ErrShortBuffer
	}
	copy(b[:virtioNetHdrLen], unsafe.Slice((*byte)(unsafe.Pointer(v)), virtioNetHdrLen))
	return nil
}

const (
	// virtioNetHdrLen is the length in bytes of virtioNetHdr. This matches the
	// shape of the C ABI for its kernel counterpart -- sizeof(virtio_net_hdr).
	virtioNetHdrLen = int(unsafe.Sizeof(virtioNetHdr{}))

	// virtioNetHdrTnlLen is the length in bytes of virtioNetHdrTnl, the
	// extended header the kernel uses as soon as the UDP tunnel offloads of a
	// tun device are enabled. This matches the shape of the C ABI for its
	// kernel counterpart -- sizeof(virtio_net_hdr_v1_hash_tunnel).
	virtioNetHdrTnlLen = 24

	// The offsets of the tunnel specific fields of virtioNetHdrTnl.
	outerThOffsetAt = 20
	innerNhOffsetAt = 22
)

// The following are defined in the kernel in include/uapi/linux/virtio_net.h,
// where golang.org/x/sys/unix has not caught up with them yet.
const (
	virtioNetHdrFUDPTunnelCSum   uint8 = 0x8  // VIRTIO_NET_HDR_F_UDP_TUNNEL_CSUM
	virtioNetHdrGSOUDPTunnelIPv4 uint8 = 0x20 // VIRTIO_NET_HDR_GSO_UDP_TUNNEL_IPV4
	virtioNetHdrGSOUDPTunnelIPv6 uint8 = 0x40 // VIRTIO_NET_HDR_GSO_UDP_TUNNEL_IPV6
)

const virtioNetHdrGSOUDPTunnel uint8 = virtioNetHdrGSOUDPTunnelIPv4 | virtioNetHdrGSOUDPTunnelIPv6

// virtioNetHdrTnl is the kernel's virtio_net_hdr_v1_hash_tunnel. A GSO packet
// carrying a GSO packet encapsulated by a UDP tunnel -- VxLAN, GENEVE, and
// friends -- cannot be described by virtioNetHdr alone: the kernel also has to
// tell us where the encapsulating headers sit within the packet.
type virtioNetHdrTnl struct {
	virtioNetHdr
	outerThOffset uint16 // offset of the encapsulating transport header
	innerNhOffset uint16 // offset of the encapsulated network header
}

func (v *virtioNetHdrTnl) decode(b []byte) error {
	if err := v.virtioNetHdr.decode(b); err != nil {
		return err
	}
	if len(b) < virtioNetHdrTnlLen {
		return io.ErrShortBuffer
	}
	v.outerThOffset = binary.LittleEndian.Uint16(b[outerThOffsetAt:])
	v.innerNhOffset = binary.LittleEndian.Uint16(b[innerNhOffsetAt:])
	return nil
}

func (v *virtioNetHdrTnl) encode(b []byte) error {
	if err := v.virtioNetHdr.encode(b); err != nil {
		return err
	}
	if len(b) < virtioNetHdrTnlLen {
		return io.ErrShortBuffer
	}
	// The fields between the two headers are the hash report and the flow hash,
	// neither of which the kernel ever fills in for us.
	binary.LittleEndian.PutUint16(b[outerThOffsetAt:], v.outerThOffset)
	binary.LittleEndian.PutUint16(b[innerNhOffsetAt:], v.innerNhOffset)
	return nil
}

// tcpFlowKey represents the key for a TCP flow.
type tcpFlowKey struct {
	srcAddr, dstAddr [16]byte
	srcPort, dstPort uint16
	rxAck            uint32 // varying ack values should not be coalesced. Treat them as separate flows.
	isV6             bool
}

// tcpGROTable holds flow and coalescing information for the purposes of TCP GRO.
type tcpGROTable struct {
	itemsByFlow map[tcpFlowKey][]tcpGROItem
	itemsPool   [][]tcpGROItem
}

func newTCPGROTable() *tcpGROTable {
	t := &tcpGROTable{
		itemsByFlow: make(map[tcpFlowKey][]tcpGROItem, conn.IdealBatchSize),
		itemsPool:   make([][]tcpGROItem, conn.IdealBatchSize),
	}
	for i := range t.itemsPool {
		t.itemsPool[i] = make([]tcpGROItem, 0, conn.IdealBatchSize)
	}
	return t
}

func newTCPFlowKey(pkt []byte, srcAddrOffset, dstAddrOffset, tcphOffset int) tcpFlowKey {
	key := tcpFlowKey{}
	addrSize := dstAddrOffset - srcAddrOffset
	copy(key.srcAddr[:], pkt[srcAddrOffset:dstAddrOffset])
	copy(key.dstAddr[:], pkt[dstAddrOffset:dstAddrOffset+addrSize])
	key.srcPort = binary.BigEndian.Uint16(pkt[tcphOffset:])
	key.dstPort = binary.BigEndian.Uint16(pkt[tcphOffset+2:])
	key.rxAck = binary.BigEndian.Uint32(pkt[tcphOffset+8:])
	key.isV6 = addrSize == 16
	return key
}

// lookupOrInsert looks up a flow for the provided packet and metadata,
// returning the packets found for the flow, or inserting a new one if none
// is found.
func (t *tcpGROTable) lookupOrInsert(pkt []byte, srcAddrOffset, dstAddrOffset, tcphOffset, tcphLen, bufsIndex int) ([]tcpGROItem, bool) {
	key := newTCPFlowKey(pkt, srcAddrOffset, dstAddrOffset, tcphOffset)
	items, ok := t.itemsByFlow[key]
	if ok {
		return items, ok
	}
	// TODO: insert() performs another map lookup. This could be rearranged to avoid.
	t.insert(pkt, srcAddrOffset, dstAddrOffset, tcphOffset, tcphLen, bufsIndex)
	return nil, false
}

// insert an item in the table for the provided packet and packet metadata.
func (t *tcpGROTable) insert(pkt []byte, srcAddrOffset, dstAddrOffset, tcphOffset, tcphLen, bufsIndex int) {
	key := newTCPFlowKey(pkt, srcAddrOffset, dstAddrOffset, tcphOffset)
	item := tcpGROItem{
		key:       key,
		bufsIndex: uint16(bufsIndex),
		gsoSize:   uint16(len(pkt[tcphOffset+tcphLen:])),
		iphLen:    uint8(tcphOffset),
		tcphLen:   uint8(tcphLen),
		sentSeq:   binary.BigEndian.Uint32(pkt[tcphOffset+4:]),
		pshSet:    pkt[tcphOffset+tcpFlagsOffset]&tcpFlagPSH != 0,
	}
	items, ok := t.itemsByFlow[key]
	if !ok {
		items = t.newItems()
	}
	items = append(items, item)
	t.itemsByFlow[key] = items
}

func (t *tcpGROTable) updateAt(item tcpGROItem, i int) {
	items, _ := t.itemsByFlow[item.key]
	items[i] = item
}

func (t *tcpGROTable) deleteAt(key tcpFlowKey, i int) {
	items, _ := t.itemsByFlow[key]
	items = append(items[:i], items[i+1:]...)
	t.itemsByFlow[key] = items
}

// tcpGROItem represents bookkeeping data for a TCP packet during the lifetime
// of a GRO evaluation across a vector of packets.
type tcpGROItem struct {
	key       tcpFlowKey
	sentSeq   uint32 // the sequence number
	bufsIndex uint16 // the index into the original bufs slice
	numMerged uint16 // the number of packets merged into this item
	gsoSize   uint16 // payload size
	iphLen    uint8  // ip header len
	tcphLen   uint8  // tcp header len
	pshSet    bool   // psh flag is set
}

func (t *tcpGROTable) newItems() []tcpGROItem {
	var items []tcpGROItem
	items, t.itemsPool = t.itemsPool[len(t.itemsPool)-1], t.itemsPool[:len(t.itemsPool)-1]
	return items
}

func (t *tcpGROTable) reset() {
	for k, items := range t.itemsByFlow {
		items = items[:0]
		t.itemsPool = append(t.itemsPool, items)
		delete(t.itemsByFlow, k)
	}
}

// udpFlowKey represents the key for a UDP flow.
type udpFlowKey struct {
	srcAddr, dstAddr [16]byte
	srcPort, dstPort uint16
	isV6             bool
}

// udpGROTable holds flow and coalescing information for the purposes of UDP GRO.
type udpGROTable struct {
	itemsByFlow map[udpFlowKey][]udpGROItem
	itemsPool   [][]udpGROItem
}

func newUDPGROTable() *udpGROTable {
	u := &udpGROTable{
		itemsByFlow: make(map[udpFlowKey][]udpGROItem, conn.IdealBatchSize),
		itemsPool:   make([][]udpGROItem, conn.IdealBatchSize),
	}
	for i := range u.itemsPool {
		u.itemsPool[i] = make([]udpGROItem, 0, conn.IdealBatchSize)
	}
	return u
}

func newUDPFlowKey(pkt []byte, srcAddrOffset, dstAddrOffset, udphOffset int) udpFlowKey {
	key := udpFlowKey{}
	addrSize := dstAddrOffset - srcAddrOffset
	copy(key.srcAddr[:], pkt[srcAddrOffset:dstAddrOffset])
	copy(key.dstAddr[:], pkt[dstAddrOffset:dstAddrOffset+addrSize])
	key.srcPort = binary.BigEndian.Uint16(pkt[udphOffset:])
	key.dstPort = binary.BigEndian.Uint16(pkt[udphOffset+2:])
	key.isV6 = addrSize == 16
	return key
}

// lookupOrInsert looks up a flow for the provided packet and metadata,
// returning the packets found for the flow, or inserting a new one if none
// is found.
func (u *udpGROTable) lookupOrInsert(pkt []byte, srcAddrOffset, dstAddrOffset, udphOffset, bufsIndex int) ([]udpGROItem, bool) {
	key := newUDPFlowKey(pkt, srcAddrOffset, dstAddrOffset, udphOffset)
	items, ok := u.itemsByFlow[key]
	if ok {
		return items, ok
	}
	// TODO: insert() performs another map lookup. This could be rearranged to avoid.
	u.insert(pkt, srcAddrOffset, dstAddrOffset, udphOffset, bufsIndex, false)
	return nil, false
}

// insert an item in the table for the provided packet and packet metadata.
func (u *udpGROTable) insert(pkt []byte, srcAddrOffset, dstAddrOffset, udphOffset, bufsIndex int, cSumKnownInvalid bool) {
	key := newUDPFlowKey(pkt, srcAddrOffset, dstAddrOffset, udphOffset)
	item := udpGROItem{
		key:              key,
		bufsIndex:        uint16(bufsIndex),
		gsoSize:          uint16(len(pkt[udphOffset+udphLen:])),
		iphLen:           uint8(udphOffset),
		cSumKnownInvalid: cSumKnownInvalid,
	}
	items, ok := u.itemsByFlow[key]
	if !ok {
		items = u.newItems()
	}
	items = append(items, item)
	u.itemsByFlow[key] = items
}

func (u *udpGROTable) updateAt(item udpGROItem, i int) {
	items, _ := u.itemsByFlow[item.key]
	items[i] = item
}

// udpGROItem represents bookkeeping data for a UDP packet during the lifetime
// of a GRO evaluation across a vector of packets.
type udpGROItem struct {
	key              udpFlowKey
	bufsIndex        uint16 // the index into the original bufs slice
	numMerged        uint16 // the number of packets merged into this item
	gsoSize          uint16 // payload size
	iphLen           uint8  // ip header len
	cSumKnownInvalid bool   // UDP header checksum validity; a false value DOES NOT imply valid, just unknown.
}

func (u *udpGROTable) newItems() []udpGROItem {
	var items []udpGROItem
	items, u.itemsPool = u.itemsPool[len(u.itemsPool)-1], u.itemsPool[:len(u.itemsPool)-1]
	return items
}

func (u *udpGROTable) reset() {
	for k, items := range u.itemsByFlow {
		items = items[:0]
		u.itemsPool = append(u.itemsPool, items)
		delete(u.itemsByFlow, k)
	}
}

// canCoalesce represents the outcome of checking if two TCP packets are
// candidates for coalescing.
type canCoalesce int

const (
	coalescePrepend     canCoalesce = -1
	coalesceUnavailable canCoalesce = 0
	coalesceAppend      canCoalesce = 1
)

// ipHeadersCanCoalesce returns true if the IP headers found in pktA and pktB
// meet all requirements to be merged as part of a GRO operation, otherwise it
// returns false.
func ipHeadersCanCoalesce(pktA, pktB []byte) bool {
	if len(pktA) < 9 || len(pktB) < 9 {
		return false
	}
	if pktA[0]>>4 == 6 {
		if pktA[0] != pktB[0] || pktA[1]>>4 != pktB[1]>>4 {
			// cannot coalesce with unequal Traffic class values
			return false
		}
		if pktA[7] != pktB[7] {
			// cannot coalesce with unequal Hop limit values
			return false
		}
	} else {
		if pktA[1] != pktB[1] {
			// cannot coalesce with unequal ToS values
			return false
		}
		if pktA[6]>>5 != pktB[6]>>5 {
			// cannot coalesce with unequal DF or reserved bits. MF is checked
			// further up the stack.
			return false
		}
		if pktA[8] != pktB[8] {
			// cannot coalesce with unequal TTL values
			return false
		}
	}
	return true
}

// udpPacketsCanCoalesce evaluates if pkt can be coalesced with the packet
// described by item. iphLen and gsoSize describe pkt. bufs is the vector of
// packets involved in the current GRO evaluation. bufsOffset is the offset at
// which packet data begins within bufs.
func udpPacketsCanCoalesce(pkt []byte, iphLen uint8, gsoSize uint16, item udpGROItem, bufs [][]byte, bufsOffset int) canCoalesce {
	pktTarget := bufs[item.bufsIndex][bufsOffset:]
	if !ipHeadersCanCoalesce(pkt, pktTarget) {
		return coalesceUnavailable
	}
	if len(pktTarget[iphLen+udphLen:])%int(item.gsoSize) != 0 {
		// A smaller than gsoSize packet has been appended previously.
		// Nothing can come after a smaller packet on the end.
		return coalesceUnavailable
	}
	if gsoSize > item.gsoSize {
		// We cannot have a larger packet following a smaller one.
		return coalesceUnavailable
	}
	return coalesceAppend
}

// tcpPacketsCanCoalesce evaluates if pkt can be coalesced with the packet
// described by item. This function makes considerations that match the kernel's
// GRO self tests, which can be found in tools/testing/selftests/net/gro.c.
func tcpPacketsCanCoalesce(pkt []byte, iphLen, tcphLen uint8, seq uint32, pshSet bool, gsoSize uint16, item tcpGROItem, bufs [][]byte, bufsOffset int) canCoalesce {
	pktTarget := bufs[item.bufsIndex][bufsOffset:]
	if tcphLen != item.tcphLen {
		// cannot coalesce with unequal tcp options len
		return coalesceUnavailable
	}
	if tcphLen > 20 {
		if !bytes.Equal(pkt[iphLen+20:iphLen+tcphLen], pktTarget[item.iphLen+20:iphLen+tcphLen]) {
			// cannot coalesce with unequal tcp options
			return coalesceUnavailable
		}
	}
	if !ipHeadersCanCoalesce(pkt, pktTarget) {
		return coalesceUnavailable
	}
	// seq adjacency
	lhsLen := item.gsoSize
	lhsLen += item.numMerged * item.gsoSize
	if seq == item.sentSeq+uint32(lhsLen) { // pkt aligns following item from a seq num perspective
		if item.pshSet {
			// We cannot append to a segment that has the PSH flag set, PSH
			// can only be set on the final segment in a reassembled group.
			return coalesceUnavailable
		}
		if len(pktTarget[iphLen+tcphLen:])%int(item.gsoSize) != 0 {
			// A smaller than gsoSize packet has been appended previously.
			// Nothing can come after a smaller packet on the end.
			return coalesceUnavailable
		}
		if gsoSize > item.gsoSize {
			// We cannot have a larger packet following a smaller one.
			return coalesceUnavailable
		}
		return coalesceAppend
	} else if seq+uint32(gsoSize) == item.sentSeq { // pkt aligns in front of item from a seq num perspective
		if pshSet {
			// We cannot prepend with a segment that has the PSH flag set, PSH
			// can only be set on the final segment in a reassembled group.
			return coalesceUnavailable
		}
		if gsoSize < item.gsoSize {
			// We cannot have a larger packet following a smaller one.
			return coalesceUnavailable
		}
		if gsoSize > item.gsoSize && item.numMerged > 0 {
			// There's at least one previous merge, and we're larger than all
			// previous. This would put multiple smaller packets on the end.
			return coalesceUnavailable
		}
		return coalescePrepend
	}
	return coalesceUnavailable
}

func checksumValid(pkt []byte, iphLen, proto uint8, isV6 bool) bool {
	return checksumValidAt(pkt, 0, int(iphLen), proto, isV6)
}

// checksumValidAt is checksumValid for an IP header that does not begin at the
// front of pkt, which is what an encapsulated packet needs.
func checksumValidAt(pkt []byte, iphAt, iphLen int, proto uint8, isV6 bool) bool {
	if iphAt < 0 || iphAt+iphLen > len(pkt) {
		return false
	}
	srcAddrAt := iphAt + ipv4SrcAddrOffset
	addrSize := 4
	if isV6 {
		srcAddrAt = iphAt + ipv6SrcAddrOffset
		addrSize = 16
	}
	if srcAddrAt+addrSize*2 > len(pkt) {
		return false
	}
	lenForPseudo := uint16(len(pkt) - iphAt - int(iphLen))
	srcAddr := pkt[srcAddrAt : srcAddrAt+addrSize]
	dstAddr := pkt[srcAddrAt+addrSize : srcAddrAt+addrSize*2]
	cSum := pseudoHeaderChecksumNoFold(proto, srcAddr, dstAddr, lenForPseudo)
	return ^checksum(pkt[iphAt+iphLen:], cSum) == 0
}

// coalesceResult represents the result of attempting to coalesce two TCP
// packets.
type coalesceResult int

const (
	coalesceInsufficientCap coalesceResult = iota
	coalescePSHEnding
	coalesceItemInvalidCSum
	coalescePktInvalidCSum
	coalesceSuccess
)

// coalesceUDPPackets attempts to coalesce pkt with the packet described by
// item, and returns the outcome.
func coalesceUDPPackets(pkt []byte, item *udpGROItem, bufs [][]byte, bufsOffset int, isV6 bool) coalesceResult {
	pktHead := bufs[item.bufsIndex][bufsOffset:] // the packet that will end up at the front
	headersLen := item.iphLen + udphLen
	coalescedLen := len(bufs[item.bufsIndex][bufsOffset:]) + len(pkt) - int(headersLen)

	if cap(pktHead)-bufsOffset < coalescedLen {
		// We don't want to allocate a new underlying array if capacity is
		// too small.
		return coalesceInsufficientCap
	}
	if item.numMerged == 0 {
		if item.cSumKnownInvalid || !checksumValid(bufs[item.bufsIndex][bufsOffset:], item.iphLen, unix.IPPROTO_UDP, isV6) {
			return coalesceItemInvalidCSum
		}
	}
	if !checksumValid(pkt, item.iphLen, unix.IPPROTO_UDP, isV6) {
		return coalescePktInvalidCSum
	}
	extendBy := len(pkt) - int(headersLen)
	bufs[item.bufsIndex] = append(bufs[item.bufsIndex], make([]byte, extendBy)...)
	copy(bufs[item.bufsIndex][bufsOffset+len(pktHead):], pkt[headersLen:])

	item.numMerged++
	return coalesceSuccess
}

// coalesceTCPPackets attempts to coalesce pkt with the packet described by
// item, and returns the outcome. This function may swap bufs elements in the
// event of a prepend as item's bufs index is already being tracked for writing
// to a Device.
func coalesceTCPPackets(mode canCoalesce, pkt []byte, pktBuffsIndex int, gsoSize uint16, seq uint32, pshSet bool, item *tcpGROItem, bufs [][]byte, bufsOffset int, isV6 bool) coalesceResult {
	var pktHead []byte // the packet that will end up at the front
	headersLen := item.iphLen + item.tcphLen
	coalescedLen := len(bufs[item.bufsIndex][bufsOffset:]) + len(pkt) - int(headersLen)

	// Copy data
	if mode == coalescePrepend {
		pktHead = pkt
		if cap(pkt)-bufsOffset < coalescedLen {
			// We don't want to allocate a new underlying array if capacity is
			// too small.
			return coalesceInsufficientCap
		}
		if pshSet {
			return coalescePSHEnding
		}
		if item.numMerged == 0 {
			if !checksumValid(bufs[item.bufsIndex][bufsOffset:], item.iphLen, unix.IPPROTO_TCP, isV6) {
				return coalesceItemInvalidCSum
			}
		}
		if !checksumValid(pkt, item.iphLen, unix.IPPROTO_TCP, isV6) {
			return coalescePktInvalidCSum
		}
		item.sentSeq = seq
		extendBy := coalescedLen - len(pktHead)
		bufs[pktBuffsIndex] = append(bufs[pktBuffsIndex], make([]byte, extendBy)...)
		copy(bufs[pktBuffsIndex][bufsOffset+len(pkt):], bufs[item.bufsIndex][bufsOffset+int(headersLen):])
		// Flip the slice headers in bufs as part of prepend. The index of item
		// is already being tracked for writing.
		bufs[item.bufsIndex], bufs[pktBuffsIndex] = bufs[pktBuffsIndex], bufs[item.bufsIndex]
	} else {
		pktHead = bufs[item.bufsIndex][bufsOffset:]
		if cap(pktHead)-bufsOffset < coalescedLen {
			// We don't want to allocate a new underlying array if capacity is
			// too small.
			return coalesceInsufficientCap
		}
		if item.numMerged == 0 {
			if !checksumValid(bufs[item.bufsIndex][bufsOffset:], item.iphLen, unix.IPPROTO_TCP, isV6) {
				return coalesceItemInvalidCSum
			}
		}
		if !checksumValid(pkt, item.iphLen, unix.IPPROTO_TCP, isV6) {
			return coalescePktInvalidCSum
		}
		if pshSet {
			// We are appending a segment with PSH set.
			item.pshSet = pshSet
			pktHead[item.iphLen+tcpFlagsOffset] |= tcpFlagPSH
		}
		extendBy := len(pkt) - int(headersLen)
		bufs[item.bufsIndex] = append(bufs[item.bufsIndex], make([]byte, extendBy)...)
		copy(bufs[item.bufsIndex][bufsOffset+len(pktHead):], pkt[headersLen:])
	}

	if gsoSize > item.gsoSize {
		item.gsoSize = gsoSize
	}

	item.numMerged++
	return coalesceSuccess
}

const (
	ipv4FlagMoreFragments uint8 = 0x20
)

const (
	ipv4SrcAddrOffset = 12
	ipv6SrcAddrOffset = 8
	ipv4AddrLen       = 4
	ipv6AddrLen       = 16
	maxUint16         = 1<<16 - 1
)

type groResult int

const (
	groResultNoop groResult = iota
	groResultTableInsert
	groResultCoalesced
)

// tcpGRO evaluates the TCP packet at pktI in bufs for coalescing with
// existing packets tracked in table. It returns a groResultNoop when no
// action was taken, groResultTableInsert when the evaluated packet was
// inserted into table, and groResultCoalesced when the evaluated packet was
// coalesced with another packet in table.
func tcpGRO(bufs [][]byte, offset int, pktI int, table *tcpGROTable, isV6 bool) groResult {
	pkt := bufs[pktI][offset:]
	if len(pkt) > maxUint16 {
		// A valid IPv4 or IPv6 packet will never exceed this.
		return groResultNoop
	}
	iphLen := int((pkt[0] & 0x0F) * 4)
	if isV6 {
		iphLen = 40
		ipv6HPayloadLen := int(binary.BigEndian.Uint16(pkt[4:]))
		if ipv6HPayloadLen != len(pkt)-iphLen {
			return groResultNoop
		}
	} else {
		totalLen := int(binary.BigEndian.Uint16(pkt[2:]))
		if totalLen != len(pkt) {
			return groResultNoop
		}
	}
	if len(pkt) < iphLen {
		return groResultNoop
	}
	tcphLen := int((pkt[iphLen+12] >> 4) * 4)
	if tcphLen < 20 || tcphLen > 60 {
		return groResultNoop
	}
	if len(pkt) < iphLen+tcphLen {
		return groResultNoop
	}
	if !isV6 {
		if pkt[6]&ipv4FlagMoreFragments != 0 || pkt[6]<<3 != 0 || pkt[7] != 0 {
			// no GRO support for fragmented segments for now
			return groResultNoop
		}
	}
	tcpFlags := pkt[iphLen+tcpFlagsOffset]
	var pshSet bool
	// not a candidate if any non-ACK flags (except PSH+ACK) are set
	if tcpFlags != tcpFlagACK {
		if pkt[iphLen+tcpFlagsOffset] != tcpFlagACK|tcpFlagPSH {
			return groResultNoop
		}
		pshSet = true
	}
	gsoSize := uint16(len(pkt) - tcphLen - iphLen)
	// not a candidate if payload len is 0
	if gsoSize < 1 {
		return groResultNoop
	}
	seq := binary.BigEndian.Uint32(pkt[iphLen+4:])
	srcAddrOffset := ipv4SrcAddrOffset
	addrLen := 4
	if isV6 {
		srcAddrOffset = ipv6SrcAddrOffset
		addrLen = 16
	}
	items, existing := table.lookupOrInsert(pkt, srcAddrOffset, srcAddrOffset+addrLen, iphLen, tcphLen, pktI)
	if !existing {
		return groResultTableInsert
	}
	for i := len(items) - 1; i >= 0; i-- {
		// In the best case of packets arriving in order iterating in reverse is
		// more efficient if there are multiple items for a given flow. This
		// also enables a natural table.deleteAt() in the
		// coalesceItemInvalidCSum case without the need for index tracking.
		// This algorithm makes a best effort to coalesce in the event of
		// unordered packets, where pkt may land anywhere in items from a
		// sequence number perspective, however once an item is inserted into
		// the table it is never compared across other items later.
		item := items[i]
		can := tcpPacketsCanCoalesce(pkt, uint8(iphLen), uint8(tcphLen), seq, pshSet, gsoSize, item, bufs, offset)
		if can != coalesceUnavailable {
			result := coalesceTCPPackets(can, pkt, pktI, gsoSize, seq, pshSet, &item, bufs, offset, isV6)
			switch result {
			case coalesceSuccess:
				table.updateAt(item, i)
				return groResultCoalesced
			case coalesceItemInvalidCSum:
				// delete the item with an invalid csum
				table.deleteAt(item.key, i)
				// The deleted item will not be re-visited in applyTCPCoalesceAccounting,
				// so we must zero the virtioNetHdr. clear() is the equivalent of
				// encoding a zero value virtioNetHdr.
				clear(bufs[item.bufsIndex][offset-virtioNetHdrLen : offset])
			case coalescePktInvalidCSum:
				// no point in inserting an item that we can't coalesce
				return groResultNoop
			default:
			}
		}
	}
	// failed to coalesce with any other packets; store the item in the flow
	table.insert(pkt, srcAddrOffset, srcAddrOffset+addrLen, iphLen, tcphLen, pktI)
	return groResultTableInsert
}

// groHdrStore is where the virtio net header of each packet is left for the
// write to pick up. When the kernel expects a 10 byte header that is the space
// immediately before the packet in its own buffer. When it expects the larger
// tunnel header it is a scratch area instead, because the space a Device hands
// handleGRO in front of each packet is only 16 bytes and the tunnel header is
// 24.
type groHdrStore struct {
	bufs    [][]byte
	offset  int
	scratch [][]byte // nil when the space before the packet is used
}

// at returns the 10 byte header slot of packet i.
func (s *groHdrStore) at(i int) []byte {
	if s.scratch != nil {
		return s.scratch[i][:virtioNetHdrLen:virtioNetHdrLen]
	}
	return s.bufs[i][s.offset-virtioNetHdrLen : s.offset]
}

// tnlAt returns the full header slot of packet i, which is only wide enough for
// a tunnel header when a scratch area is in use.
func (s *groHdrStore) tnlAt(i int) []byte {
	if s.scratch != nil {
		return s.scratch[i][:virtioNetHdrTnlLen:virtioNetHdrTnlLen]
	}
	return s.bufs[i][s.offset-virtioNetHdrLen : s.offset]
}

// zero clears the whole header slot of packet i, leaving it as a plain packet
// with no offload.
func (s *groHdrStore) zero(i int) {
	if s.scratch != nil {
		clear(s.scratch[i])
		return
	}
	clear(s.bufs[i][s.offset-virtioNetHdrLen : s.offset])
}

// applyTCPCoalesceAccounting updates bufs to account for coalescing based on the
// metadata found in table.
func applyTCPCoalesceAccounting(bufs [][]byte, offset int, table *tcpGROTable, store *groHdrStore) error {
	for _, items := range table.itemsByFlow {
		for _, item := range items {
			if item.numMerged > 0 {
				hdr := virtioNetHdr{
					flags:      unix.VIRTIO_NET_HDR_F_NEEDS_CSUM, // this turns into CHECKSUM_PARTIAL in the skb
					hdrLen:     uint16(item.iphLen + item.tcphLen),
					gsoSize:    item.gsoSize,
					csumStart:  uint16(item.iphLen),
					csumOffset: 16,
				}
				pkt := bufs[item.bufsIndex][offset:]

				// Recalculate the total len (IPv4) or payload len (IPv6).
				// Recalculate the (IPv4) header checksum.
				if item.key.isV6 {
					hdr.gsoType = unix.VIRTIO_NET_HDR_GSO_TCPV6
					binary.BigEndian.PutUint16(pkt[4:], uint16(len(pkt))-uint16(item.iphLen)) // set new IPv6 header payload len
				} else {
					hdr.gsoType = unix.VIRTIO_NET_HDR_GSO_TCPV4
					pkt[10], pkt[11] = 0, 0
					binary.BigEndian.PutUint16(pkt[2:], uint16(len(pkt))) // set new total length
					iphCSum := ^checksum(pkt[:item.iphLen], 0)            // compute IPv4 header checksum
					binary.BigEndian.PutUint16(pkt[10:], iphCSum)         // set IPv4 header checksum field
				}
				err := hdr.encode(store.at(int(item.bufsIndex)))
				if err != nil {
					return err
				}

				// Calculate the pseudo header checksum and place it at the TCP
				// checksum offset. Downstream checksum offloading will combine
				// this with computation of the tcp header and payload checksum.
				addrLen := 4
				addrOffset := ipv4SrcAddrOffset
				if item.key.isV6 {
					addrLen = 16
					addrOffset = ipv6SrcAddrOffset
				}
				srcAddrAt := offset + addrOffset
				srcAddr := bufs[item.bufsIndex][srcAddrAt : srcAddrAt+addrLen]
				dstAddr := bufs[item.bufsIndex][srcAddrAt+addrLen : srcAddrAt+addrLen*2]
				psum := pseudoHeaderChecksumNoFold(unix.IPPROTO_TCP, srcAddr, dstAddr, uint16(len(pkt)-int(item.iphLen)))
				binary.BigEndian.PutUint16(pkt[hdr.csumStart+hdr.csumOffset:], checksum([]byte{}, psum))
			}
		}
	}
	return nil
}

// applyUDPCoalesceAccounting updates bufs to account for coalescing based on the
// metadata found in table.
func applyUDPCoalesceAccounting(bufs [][]byte, offset int, table *udpGROTable, store *groHdrStore) error {
	for _, items := range table.itemsByFlow {
		for _, item := range items {
			if item.numMerged > 0 {
				hdr := virtioNetHdr{
					flags:      unix.VIRTIO_NET_HDR_F_NEEDS_CSUM, // this turns into CHECKSUM_PARTIAL in the skb
					hdrLen:     uint16(item.iphLen + udphLen),
					gsoSize:    item.gsoSize,
					csumStart:  uint16(item.iphLen),
					csumOffset: 6,
				}
				pkt := bufs[item.bufsIndex][offset:]

				// Recalculate the total len (IPv4) or payload len (IPv6).
				// Recalculate the (IPv4) header checksum.
				hdr.gsoType = unix.VIRTIO_NET_HDR_GSO_UDP_L4
				if item.key.isV6 {
					binary.BigEndian.PutUint16(pkt[4:], uint16(len(pkt))-uint16(item.iphLen)) // set new IPv6 header payload len
				} else {
					pkt[10], pkt[11] = 0, 0
					binary.BigEndian.PutUint16(pkt[2:], uint16(len(pkt))) // set new total length
					iphCSum := ^checksum(pkt[:item.iphLen], 0)            // compute IPv4 header checksum
					binary.BigEndian.PutUint16(pkt[10:], iphCSum)         // set IPv4 header checksum field
				}
				err := hdr.encode(store.at(int(item.bufsIndex)))
				if err != nil {
					return err
				}

				// Recalculate the UDP len field value
				binary.BigEndian.PutUint16(pkt[item.iphLen+4:], uint16(len(pkt[item.iphLen:])))

				// Calculate the pseudo header checksum and place it at the UDP
				// checksum offset. Downstream checksum offloading will combine
				// this with computation of the udp header and payload checksum.
				addrLen := 4
				addrOffset := ipv4SrcAddrOffset
				if item.key.isV6 {
					addrLen = 16
					addrOffset = ipv6SrcAddrOffset
				}
				srcAddrAt := offset + addrOffset
				srcAddr := bufs[item.bufsIndex][srcAddrAt : srcAddrAt+addrLen]
				dstAddr := bufs[item.bufsIndex][srcAddrAt+addrLen : srcAddrAt+addrLen*2]
				psum := pseudoHeaderChecksumNoFold(unix.IPPROTO_UDP, srcAddr, dstAddr, uint16(len(pkt)-int(item.iphLen)))
				binary.BigEndian.PutUint16(pkt[hdr.csumStart+hdr.csumOffset:], checksum([]byte{}, psum))
			}
		}
	}
	return nil
}

type groCandidateType uint8

const (
	notGROCandidate groCandidateType = iota
	tcp4GROCandidate
	tcp6GROCandidate
	udp4GROCandidate
	udp6GROCandidate
	vxlan4GROCandidate
	vxlan6GROCandidate
)

// minTnlLen is the smallest packet that could hold an IPv4 tunnel header, a
// VxLAN header, an Ethernet header, an IPv4 header and a TCP header. IPv6
// outer headers are larger still, but this is only a first filter: parseTunnel
// does the exact bounds checking.
const minTnlLen = ipv4HeaderLen + udphLen + vxlanLen + ethHdrLen + ipv4HeaderLen + 20

func packetIsGROCandidate(b []byte, canUDPGRO, canTunnelGRO bool) groCandidateType {
	if len(b) < 28 {
		return notGROCandidate
	}
	if b[0]>>4 == 4 {
		if b[0]&0x0F != 5 {
			// IPv4 packets w/IP options do not coalesce
			return notGROCandidate
		}
		if b[9] == unix.IPPROTO_TCP && len(b) >= 40 {
			return tcp4GROCandidate
		}
		if b[9] == unix.IPPROTO_UDP && canUDPGRO {
			if canTunnelGRO && len(b) >= minTnlLen && isVxlanPort(binary.BigEndian.Uint16(b[ipv4HeaderLen+2:])) {
				return vxlan4GROCandidate
			}
			return udp4GROCandidate
		}
	} else if b[0]>>4 == 6 {
		if b[6] == unix.IPPROTO_TCP && len(b) >= 60 {
			return tcp6GROCandidate
		}
		if b[6] == unix.IPPROTO_UDP && len(b) >= 48 && canUDPGRO {
			if canTunnelGRO && len(b) >= minTnlLen+ipv6HeaderLen-ipv4HeaderLen && isVxlanPort(binary.BigEndian.Uint16(b[ipv6HeaderLen+2:])) {
				return vxlan6GROCandidate
			}
			return udp6GROCandidate
		}
	}
	return notGROCandidate
}

const (
	ipv4HeaderLen = 20
	ipv6HeaderLen = 40
	udphLen       = 8
	vxlanLen      = 8 // the fixed part of a VxLAN or VxLAN-GPE header
)

// udpGRO evaluates the UDP packet at pktI in bufs for coalescing with
// existing packets tracked in table. It returns a groResultNoop when no
// action was taken, groResultTableInsert when the evaluated packet was
// inserted into table, and groResultCoalesced when the evaluated packet was
// coalesced with another packet in table.
func udpGRO(bufs [][]byte, offset int, pktI int, table *udpGROTable, isV6 bool) groResult {
	pkt := bufs[pktI][offset:]
	if len(pkt) > maxUint16 {
		// A valid IPv4 or IPv6 packet will never exceed this.
		return groResultNoop
	}
	iphLen := int((pkt[0] & 0x0F) * 4)
	if isV6 {
		iphLen = 40
		ipv6HPayloadLen := int(binary.BigEndian.Uint16(pkt[4:]))
		if ipv6HPayloadLen != len(pkt)-iphLen {
			return groResultNoop
		}
	} else {
		totalLen := int(binary.BigEndian.Uint16(pkt[2:]))
		if totalLen != len(pkt) {
			return groResultNoop
		}
	}
	if len(pkt) < iphLen {
		return groResultNoop
	}
	if len(pkt) < iphLen+udphLen {
		return groResultNoop
	}
	if !isV6 {
		if pkt[6]&ipv4FlagMoreFragments != 0 || pkt[6]<<3 != 0 || pkt[7] != 0 {
			// no GRO support for fragmented segments for now
			return groResultNoop
		}
	}
	gsoSize := uint16(len(pkt) - udphLen - iphLen)
	// not a candidate if payload len is 0
	if gsoSize < 1 {
		return groResultNoop
	}
	srcAddrOffset := ipv4SrcAddrOffset
	addrLen := 4
	if isV6 {
		srcAddrOffset = ipv6SrcAddrOffset
		addrLen = 16
	}
	items, existing := table.lookupOrInsert(pkt, srcAddrOffset, srcAddrOffset+addrLen, iphLen, pktI)
	if !existing {
		return groResultTableInsert
	}
	// With UDP we only check the last item, otherwise we could reorder packets
	// for a given flow. We must also always insert a new item, or successfully
	// coalesce with an existing item, for the same reason.
	item := items[len(items)-1]
	can := udpPacketsCanCoalesce(pkt, uint8(iphLen), gsoSize, item, bufs, offset)
	var pktCSumKnownInvalid bool
	if can == coalesceAppend {
		result := coalesceUDPPackets(pkt, &item, bufs, offset, isV6)
		switch result {
		case coalesceSuccess:
			table.updateAt(item, len(items)-1)
			return groResultCoalesced
		case coalesceItemInvalidCSum:
			// If the existing item has an invalid csum we take no action. A new
			// item will be stored after it, and the existing item will never be
			// revisited as part of future coalescing candidacy checks.
		case coalescePktInvalidCSum:
			// We must insert a new item, but we also mark it as invalid csum
			// to prevent a repeat checksum validation.
			pktCSumKnownInvalid = true
		default:
		}
	}
	// failed to coalesce with any other packets; store the item in the flow
	table.insert(pkt, srcAddrOffset, srcAddrOffset+addrLen, iphLen, pktI, pktCSumKnownInvalid)
	return groResultTableInsert
}

// handleGRO evaluates bufs for GRO, and writes the indices of the resulting
// packets into toWrite. toWrite, tcpTable, and udpTable should initially be
// empty (but non-nil), and are passed in to save allocs as the caller may reset
// and recycle them across vectors of packets. canUDPGRO indicates if UDP GRO is
// supported. tnlTable is nil when the kernel will not accept a tunnel GSO
// header, in which case encapsulated packets are coalesced as plain UDP. When
// hdrs is non-nil it is a scratch area, one virtioNetHdrTnlLen sized slot per
// buffer, that holds the headers instead of the space in front of each packet.
func handleGRO(bufs [][]byte, offset int, tcpTable *tcpGROTable, udpTable *udpGROTable, tnlTable *tunnelGROTable, canUDPGRO bool, hdrs [][]byte, toWrite *[]int) error {
	store := &groHdrStore{bufs: bufs, offset: offset, scratch: hdrs}
	for i := range bufs {
		if offset < virtioNetHdrLen || offset > len(bufs[i])-1 {
			return errors.New("invalid offset")
		}
		var result groResult
		switch candidate := packetIsGROCandidate(bufs[i][offset:], canUDPGRO, tnlTable != nil); candidate {
		case tcp4GROCandidate:
			result = tcpGRO(bufs, offset, i, tcpTable, false)
		case tcp6GROCandidate:
			result = tcpGRO(bufs, offset, i, tcpTable, true)
		case udp4GROCandidate:
			result = udpGRO(bufs, offset, i, udpTable, false)
		case udp6GROCandidate:
			result = udpGRO(bufs, offset, i, udpTable, true)
		case vxlan4GROCandidate, vxlan6GROCandidate:
			result = tunnelGRO(bufs, offset, i, tnlTable)
			if result == groResultNoop {
				// An encapsulated packet we cannot describe as tunnel GSO is still
				// a plain UDP datagram, so fall back to coalescing it as one.
				result = udpGRO(bufs, offset, i, udpTable, candidate == vxlan6GROCandidate)
			}
		}
		switch result {
		case groResultNoop, groResultTableInsert:
			// The packet is written as it arrived, so its header has to describe
			// no offload at all. A packet that later merges into a group gets its
			// header rewritten by the accounting below.
			store.zero(i)
			*toWrite = append(*toWrite, i)
		}
	}
	errTCP := applyTCPCoalesceAccounting(bufs, offset, tcpTable, store)
	errUDP := applyUDPCoalesceAccounting(bufs, offset, udpTable, store)
	errTnl := applyTunnelCoalesceAccounting(bufs, offset, tnlTable, store)
	return errors.Join(errTCP, errUDP, errTnl)
}

// gsoLayout describes the headers of a GSO packet, so that the packet can be
// split into segments. The transport header described by transportAt is the one
// whose checksum GSO offload leaves for us to compute; for a packet that is not
// encapsulated it is also the outermost transport header, and so the outer and
// inner IP headers are one and the same.
type gsoLayout struct {
	outerIphAt   int  // offset of the outermost IP header
	outerIphLen  int  // length of the outermost IP header
	outerIsV6    bool // the outermost IP header is IPv6
	outerUdpAt   int  // offset of the encapsulating UDP header, or -1
	outerCSum    bool // the encapsulating UDP checksum is ours to compute
	innerIphAt   int  // offset of the innermost IP header
	innerIphLen  int  // length of the innermost IP header
	innerIsV6    bool // the innermost IP header is IPv6
	transportAt  int  // offset of the transport header carrying the GSO checksum
	transportLen int  // length of that transport header
	csumOffset   int  // offset of the checksum field within that header
	protocol     uint8
	vxlanAt      int // offset of the encapsulating tunnel header, or -1
	innerEthAt   int // offset of the encapsulated MAC header, or -1
}

// transportHeaderLen returns the length of the transport header at transportAt
// in b, whose payload protocol is protocol.
// transportHeaderLen returns the length of the transport header at transportAt
// in b, whose payload protocol is protocol. The GSO split and the GRO paths use
// it together so that they cannot disagree about what a header is.
func transportHeaderLen(b []byte, transportAt int, protocol uint8) (int, error) {
	if protocol == unix.IPPROTO_UDP {
		if transportAt+udphLen > len(b) {
			return 0, errors.New("packet is too short")
		}
		return udphLen, nil
	}
	if transportAt+20 > len(b) {
		return 0, errors.New("packet is too short")
	}
	tcpHLen := int(b[transportAt+12]>>4) * 4
	if tcpHLen < 20 || tcpHLen > 60 {
		// A TCP header must be between 20 and 60 bytes in length.
		return 0, fmt.Errorf("tcp header len is invalid: %d", tcpHLen)
	}
	return tcpHLen, nil
}

// ipSegmentAddrs returns the source and destination addresses of the IP header
// of length iphLen at iphAt in b.
func ipSegmentAddrs(b []byte, iphAt int, isV6 bool) (src, dst []byte) {
	if isV6 {
		return b[iphAt+ipv6SrcAddrOffset : iphAt+ipv6SrcAddrOffset+ipv6AddrLen],
			b[iphAt+ipv6SrcAddrOffset+ipv6AddrLen : iphAt+ipv6SrcAddrOffset+ipv6AddrLen*2]
	}
	return b[iphAt+ipv4SrcAddrOffset : iphAt+ipv4SrcAddrOffset+ipv4AddrLen],
		b[iphAt+ipv4SrcAddrOffset+ipv4AddrLen : iphAt+ipv4SrcAddrOffset+ipv4AddrLen*2]
}

// setIPLen updates the length, identification, and checksum fields of the IP
// header of length iphLen at iphAt in b, which describes the i'th segment of a
// GSO packet whose total length is totalLen. The header checksum field of IPv4
// headers must have been cleared beforehand, in the template every segment is
// copied from.
func setIPLen(b []byte, iphAt, iphLen, totalLen, i int, isV6 bool) {
	if isV6 {
		// For IPv6 we are responsible for updating the payload length field.
		binary.BigEndian.PutUint16(b[iphAt+4:], uint16(totalLen-iphLen))
		return
	}
	// For IPv4 we are responsible for incrementing the ID field, updating the
	// total len field, and recalculating the header checksum.
	if i > 0 {
		id := binary.BigEndian.Uint16(b[iphAt+4:])
		id += uint16(i)
		binary.BigEndian.PutUint16(b[iphAt+4:], id)
	}
	binary.BigEndian.PutUint16(b[iphAt+2:], uint16(totalLen))
	ipv4CSum := ^checksum(b[iphAt:iphAt+iphLen], 0)
	binary.BigEndian.PutUint16(b[iphAt+10:], ipv4CSum)
}

// outBufLen reports how many bytes gsoSplit may write in each of the buffers it
// was handed, or a negative number when there are no usable buffers at all.
func outBufLen(outBuffs [][]byte, outOffset int) int {
	if len(outBuffs) == 0 || outOffset > len(outBuffs[0]) {
		return -1
	}
	return len(outBuffs[0]) - outOffset
}

// gsoSplit splits packets from in into outBuffs, writing the size of each
// element into sizes. It returns the number of buffers populated, and/or an
// error.
func gsoSplit(in []byte, hdr virtioNetHdr, l gsoLayout, outBuffs [][]byte, sizes []int, outOffset int) (int, error) {
	// Every segment is a copy of the template's headers plus at most gsoSize
	// bytes of payload, so the longest segment this packet can yield has to fit
	// in the buffers we were handed.
	maxSegment := int(hdr.hdrLen) + int(hdr.gsoSize)
	if len(in) < maxSegment {
		maxSegment = len(in)
	}
	if bufLen := outBufLen(outBuffs, outOffset); maxSegment > bufLen {
		return 0, fmt.Errorf("segment length (%d) exceeds output buffer length (%d)", maxSegment, bufLen)
	}

	// Clear the checksums that every segment gets its own copy of, so that the
	// headers copied from the template below do not carry stale values over.
	if !l.outerIsV6 {
		in[l.outerIphAt+10], in[l.outerIphAt+11] = 0, 0 // clear ipv4 header checksum
	}
	if l.innerIphAt != l.outerIphAt && !l.innerIsV6 {
		in[l.innerIphAt+10], in[l.innerIphAt+11] = 0, 0 // clear inner ipv4 header checksum
	}
	transportCsumAt := l.transportAt + l.csumOffset
	in[transportCsumAt], in[transportCsumAt+1] = 0, 0 // clear tcp/udp checksum
	if l.outerUdpAt >= 0 && l.outerCSum {
		in[l.outerUdpAt+6], in[l.outerUdpAt+7] = 0, 0 // clear the encapsulating udp checksum
	}

	var firstTCPSeqNum uint32
	if l.protocol == unix.IPPROTO_TCP {
		firstTCPSeqNum = binary.BigEndian.Uint32(in[l.transportAt+4:])
	}
	nextSegmentDataAt := int(hdr.hdrLen)
	i := 0
	for ; nextSegmentDataAt < len(in); i++ {
		if i == len(outBuffs) {
			return i - 1, ErrTooManySegments
		}
		nextSegmentEnd := nextSegmentDataAt + int(hdr.gsoSize)
		if nextSegmentEnd > len(in) {
			nextSegmentEnd = len(in)
		}
		segmentDataLen := nextSegmentEnd - nextSegmentDataAt
		totalLen := int(hdr.hdrLen) + segmentDataLen
		sizes[i] = totalLen
		out := outBuffs[i][outOffset:]

		// copy all headers, including the encapsulating ones
		copy(out, in[:hdr.hdrLen])

		setIPLen(out, l.outerIphAt, l.outerIphLen, totalLen, i, l.outerIsV6)
		if l.outerUdpAt >= 0 {
			// The encapsulating UDP header covers the encapsulated packet.
			binary.BigEndian.PutUint16(out[l.outerUdpAt+4:], uint16(totalLen-l.outerUdpAt))
		}
		if l.innerIphAt != l.outerIphAt {
			setIPLen(out, l.innerIphAt, l.innerIphLen, totalLen-l.innerIphAt, i, l.innerIsV6)
		}

		if l.protocol == unix.IPPROTO_TCP {
			// set TCP seq and adjust TCP flags
			tcpSeq := firstTCPSeqNum + uint32(hdr.gsoSize*uint16(i))
			binary.BigEndian.PutUint32(out[l.transportAt+4:], tcpSeq)
			if nextSegmentEnd != len(in) {
				// FIN and PSH should only be set on last segment
				clearFlags := tcpFlagFIN | tcpFlagPSH
				out[l.transportAt+tcpFlagsOffset] &^= clearFlags
			}
		} else {
			// set UDP header len
			binary.BigEndian.PutUint16(out[l.transportAt+4:], uint16(segmentDataLen)+uint16(l.transportLen))
		}

		// payload
		copy(out[hdr.hdrLen:], in[nextSegmentDataAt:nextSegmentEnd])

		// transport checksum
		srcAddr, dstAddr := ipSegmentAddrs(in, l.innerIphAt, l.innerIsV6)
		lenForPseudo := uint16(l.transportLen + segmentDataLen)
		transportCSumNoFold := pseudoHeaderChecksumNoFold(l.protocol, srcAddr, dstAddr, lenForPseudo)
		transportCSum := ^checksum(out[l.transportAt:totalLen], transportCSumNoFold)
		binary.BigEndian.PutUint16(out[l.transportAt+l.csumOffset:], transportCSum)

		// encapsulating UDP checksum
		if l.outerUdpAt >= 0 && l.outerCSum {
			srcAddr, dstAddr := ipSegmentAddrs(in, l.outerIphAt, l.outerIsV6)
			udpLen := uint16(totalLen - l.outerUdpAt)
			cSumNoFold := pseudoHeaderChecksumNoFold(unix.IPPROTO_UDP, srcAddr, dstAddr, udpLen)
			cSum := ^checksum(out[l.outerUdpAt:totalLen], cSumNoFold)
			if cSum == 0 {
				cSum = 0xffff // a zero checksum means "no checksum" in IPv4 UDP
			}
			binary.BigEndian.PutUint16(out[l.outerUdpAt+6:], cSum)
		}

		nextSegmentDataAt += int(hdr.gsoSize)
	}
	return i, nil
}

func gsoNoneChecksum(in []byte, cSumStart, cSumOffset uint16) error {
	cSumAt := int(cSumStart) + int(cSumOffset)
	if cSumAt+1 >= len(in) {
		return fmt.Errorf("end of checksum offset (%d) exceeds packet length (%d)", cSumAt+1, len(in))
	}
	// The initial value at the checksum offset should be summed with the
	// checksum we compute. This is typically the pseudo-header checksum.
	initial := binary.BigEndian.Uint16(in[cSumAt:])
	in[cSumAt], in[cSumAt+1] = 0, 0
	binary.BigEndian.PutUint16(in[cSumAt:], ^checksum(in[cSumStart:], uint64(initial)))
	return nil
}
