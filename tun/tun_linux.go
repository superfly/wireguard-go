/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2017-2025 WireGuard LLC. All Rights Reserved.
 */

package tun

/* Implementation of the TUN device interface for linux
 */

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/rwcancel"
)

const (
	cloneDevicePath = "/dev/net/tun"
	ifReqSize       = unix.IFNAMSIZ + 64
	// maxPktLen is the largest packet the kernel will hand us: the payload of a
	// GSO packet is bounded by the largest possible datagram, which a UDP tunnel
	// then wraps in its own headers.
	maxPktLen      = 65535
	maxTnlOverhead = ipv6HeaderLen + udphLen + vxlanLen
)

type NativeTun struct {
	tunFile                 *os.File
	index                   int32      // if index
	errors                  chan error // async error handling
	events                  chan Event // device related events
	netlinkSock             int
	netlinkCancel           *rwcancel.RWCancel
	hackListenerClosed      sync.Mutex
	statusListenersShutdown chan struct{}
	batchSize               int
	vnetHdr                 bool
	vnetHdrSize             int
	udpGSO                  bool

	closeOnce sync.Once

	nameOnce  sync.Once // guards calling initNameCache, which sets following fields
	nameCache string    // name of interface
	nameErr   error

	readOpMu sync.Mutex                                            // readOpMu guards readBuff
	readBuff [virtioNetHdrTnlLen + maxPktLen + maxTnlOverhead]byte // if vnetHdr every read() is prefixed by virtioNetHdr

	writeOpMu   sync.Mutex // writeOpMu guards toWrite, the GRO tables, rawConn, writeIovs
	toWrite     []int
	tcpGROTable *tcpGROTable
	udpGROTable *udpGROTable
	// tunnelGROTable coalesces packets encapsulated by a UDP tunnel. It is only
	// consulted when the kernel accepts a tunnel GSO header, which is when
	// vnetHdrSize is virtioNetHdrTnlLen.
	tunnelGROTable *tunnelGROTable
	// gsoHdrs holds the virtio net header of each packet in a batch, rather
	// than in the space in front of the packet, because that space is only
	// MessageTransportOffsetContent bytes wide and a tunnel header is wider.
	gsoHdrs     [][virtioNetHdrTnlLen]byte
	gsoHdrViews [][]byte
	rawConn     syscall.RawConn // cached handle to tunFile
	writeIovs   [2]unix.Iovec
}

func (tun *NativeTun) File() *os.File {
	return tun.tunFile
}

func (tun *NativeTun) routineHackListener() {
	defer tun.hackListenerClosed.Unlock()
	/* This is needed for the detection to work across network namespaces
	 * If you are reading this and know a better method, please get in touch.
	 */
	last := 0
	const (
		up   = 1
		down = 2
	)
	for {
		sysconn, err := tun.tunFile.SyscallConn()
		if err != nil {
			return
		}
		err2 := sysconn.Control(func(fd uintptr) {
			_, err = unix.Write(int(fd), nil)
		})
		if err2 != nil {
			return
		}
		switch err {
		case unix.EINVAL:
			if last != up {
				// If the tunnel is up, it reports that write() is
				// allowed but we provided invalid data.
				tun.events <- EventUp
				last = up
			}
		case unix.EIO:
			if last != down {
				// If the tunnel is down, it reports that no I/O
				// is possible, without checking our provided data.
				tun.events <- EventDown
				last = down
			}
		default:
			return
		}
		select {
		case <-time.After(time.Second):
			// nothing
		case <-tun.statusListenersShutdown:
			return
		}
	}
}

func createNetlinkSocket() (int, error) {
	sock, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return -1, err
	}
	saddr := &unix.SockaddrNetlink{
		Family: unix.AF_NETLINK,
		Groups: unix.RTMGRP_LINK | unix.RTMGRP_IPV4_IFADDR | unix.RTMGRP_IPV6_IFADDR,
	}
	err = unix.Bind(sock, saddr)
	if err != nil {
		return -1, err
	}
	return sock, nil
}

func (tun *NativeTun) routineNetlinkListener() {
	defer func() {
		unix.Close(tun.netlinkSock)
		tun.hackListenerClosed.Lock()
		close(tun.events)
		tun.netlinkCancel.Close()
	}()

	for msg := make([]byte, 1<<16); ; {
		var err error
		var msgn int
		for {
			msgn, _, _, _, err = unix.Recvmsg(tun.netlinkSock, msg[:], nil, 0)
			if err == nil || !rwcancel.RetryAfterError(err) {
				break
			}
			if !tun.netlinkCancel.ReadyRead() {
				tun.errors <- fmt.Errorf("netlink socket closed: %w", err)
				return
			}
		}
		if err != nil {
			tun.errors <- fmt.Errorf("failed to receive netlink message: %w", err)
			return
		}

		select {
		case <-tun.statusListenersShutdown:
			return
		default:
		}

		wasEverUp := false
		for remain := msg[:msgn]; len(remain) >= unix.SizeofNlMsghdr; {

			hdr := *(*unix.NlMsghdr)(unsafe.Pointer(&remain[0]))

			if int(hdr.Len) > len(remain) {
				break
			}

			switch hdr.Type {
			case unix.NLMSG_DONE:
				remain = []byte{}

			case unix.RTM_NEWLINK:
				info := *(*unix.IfInfomsg)(unsafe.Pointer(&remain[unix.SizeofNlMsghdr]))
				remain = remain[hdr.Len:]

				if info.Index != tun.index {
					// not our interface
					continue
				}

				if info.Flags&unix.IFF_RUNNING != 0 {
					tun.events <- EventUp
					wasEverUp = true
				}

				if info.Flags&unix.IFF_RUNNING == 0 {
					// Don't emit EventDown before we've ever emitted EventUp.
					// This avoids a startup race with HackListener, which
					// might detect Up before we have finished reporting Down.
					if wasEverUp {
						tun.events <- EventDown
					}
				}

				tun.events <- EventMTUUpdate

			default:
				remain = remain[hdr.Len:]
			}
		}
	}
}

func getIFIndex(name string) (int32, error) {
	fd, err := unix.Socket(
		unix.AF_INET,
		unix.SOCK_DGRAM|unix.SOCK_CLOEXEC,
		0,
	)
	if err != nil {
		return 0, err
	}

	defer unix.Close(fd)

	var ifr [ifReqSize]byte
	copy(ifr[:], name)
	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		uintptr(fd),
		uintptr(unix.SIOCGIFINDEX),
		uintptr(unsafe.Pointer(&ifr[0])),
	)

	if errno != 0 {
		return 0, errno
	}

	return *(*int32)(unsafe.Pointer(&ifr[unix.IFNAMSIZ])), nil
}

func (tun *NativeTun) setMTU(n int) error {
	name, err := tun.Name()
	if err != nil {
		return err
	}

	// open datagram socket
	fd, err := unix.Socket(
		unix.AF_INET,
		unix.SOCK_DGRAM|unix.SOCK_CLOEXEC,
		0,
	)
	if err != nil {
		return err
	}

	defer unix.Close(fd)

	// do ioctl call
	var ifr [ifReqSize]byte
	copy(ifr[:], name)
	*(*uint32)(unsafe.Pointer(&ifr[unix.IFNAMSIZ])) = uint32(n)
	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		uintptr(fd),
		uintptr(unix.SIOCSIFMTU),
		uintptr(unsafe.Pointer(&ifr[0])),
	)

	if errno != 0 {
		return fmt.Errorf("failed to set MTU of TUN device: %w", errno)
	}

	return nil
}

func (tun *NativeTun) MTU() (int, error) {
	name, err := tun.Name()
	if err != nil {
		return 0, err
	}

	// open datagram socket
	fd, err := unix.Socket(
		unix.AF_INET,
		unix.SOCK_DGRAM|unix.SOCK_CLOEXEC,
		0,
	)
	if err != nil {
		return 0, err
	}

	defer unix.Close(fd)

	// do ioctl call

	var ifr [ifReqSize]byte
	copy(ifr[:], name)
	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		uintptr(fd),
		uintptr(unix.SIOCGIFMTU),
		uintptr(unsafe.Pointer(&ifr[0])),
	)
	if errno != 0 {
		return 0, fmt.Errorf("failed to get MTU of TUN device: %w", errno)
	}

	return int(*(*int32)(unsafe.Pointer(&ifr[unix.IFNAMSIZ]))), nil
}

func (tun *NativeTun) Name() (string, error) {
	tun.nameOnce.Do(tun.initNameCache)
	return tun.nameCache, tun.nameErr
}

func (tun *NativeTun) initNameCache() {
	tun.nameCache, tun.nameErr = tun.nameSlow()
}

func (tun *NativeTun) nameSlow() (string, error) {
	sysconn, err := tun.tunFile.SyscallConn()
	if err != nil {
		return "", err
	}
	var ifr [ifReqSize]byte
	var errno syscall.Errno
	err = sysconn.Control(func(fd uintptr) {
		_, _, errno = unix.Syscall(
			unix.SYS_IOCTL,
			fd,
			uintptr(unix.TUNGETIFF),
			uintptr(unsafe.Pointer(&ifr[0])),
		)
	})
	if err != nil {
		return "", fmt.Errorf("failed to get name of TUN device: %w", err)
	}
	if errno != 0 {
		return "", fmt.Errorf("failed to get name of TUN device: %w", errno)
	}
	return unix.ByteSliceToString(ifr[:]), nil
}

func (tun *NativeTun) Write(bufs [][]byte, offset int) (int, error) {
	tun.writeOpMu.Lock()
	defer func() {
		tun.tcpGROTable.reset()
		tun.udpGROTable.reset()
		tun.tunnelGROTable.reset()
		tun.writeOpMu.Unlock()
	}()
	var (
		errs  error
		total int
	)
	// The kernel reads a tunnel header that no buffer has room for in front of
	// its packet, so those headers travel through a scratch area instead, and
	// every write becomes a header and a packet rather than one contiguous run
	// of bytes.
	tnlHdrs := tun.vnetHdrSize > virtioNetHdrLen
	tun.toWrite = tun.toWrite[:0]
	if tun.vnetHdr {
		var hdrs [][]byte
		var tnlTable *tunnelGROTable
		if tnlHdrs {
			hdrs = tun.groHdrScratch(len(bufs))
			tnlTable = tun.tunnelGROTable
		}
		err := handleGRO(bufs, offset, tun.tcpGROTable, tun.udpGROTable, tnlTable, tun.udpGSO, hdrs, &tun.toWrite)
		if err != nil {
			return 0, err
		}
		if !tnlHdrs {
			offset -= virtioNetHdrLen
		}
	} else {
		for i := range bufs {
			tun.toWrite = append(tun.toWrite, i)
		}
	}
	for _, bufsI := range tun.toWrite {
		var n int
		var err error
		if tnlHdrs {
			n, err = tun.writeVnetHdr(tun.gsoHdrs[bufsI][:tun.vnetHdrSize], bufs[bufsI][offset:])
		} else {
			n, err = tun.tunFile.Write(bufs[bufsI][offset:])
		}
		if errors.Is(err, syscall.EBADFD) {
			return total, os.ErrClosed
		}
		if err != nil {
			errs = errors.Join(errs, err)
		} else {
			total += n
		}
	}
	return total, errs
}

// writeVnetHdr writes a packet preceded by a virtio net header that does not
// fit in front of it, which is the case for the tunnel header: the space a
// buffer offers in front of its packet is smaller than the header the kernel
// asked us to put there, and the caller has no reason to have left room for the
// difference, so the two parts are passed to writev(2) rather than copied
// apart.
func (tun *NativeTun) writeVnetHdr(hdr, pkt []byte) (int, error) {
	if tun.rawConn == nil {
		sc, err := tun.tunFile.SyscallConn()
		if err != nil {
			return 0, err
		}
		tun.rawConn = sc
	}
	tun.writeIovs[0].Base = &hdr[0]
	tun.writeIovs[0].SetLen(len(hdr))
	tun.writeIovs[1].Base = &pkt[0]
	tun.writeIovs[1].SetLen(len(pkt))
	var written int
	var errno syscall.Errno
	// The header and the packet are described by an iovec that lives on the
	// Device, rather than one unix.Writev would allocate, because this is on
	// the path of every packet the device receives.
	err := tun.rawConn.Write(func(fd uintptr) bool {
		var r0 uintptr
		r0, _, errno = unix.Syscall(unix.SYS_WRITEV, fd, uintptr(unsafe.Pointer(&tun.writeIovs[0])), 2)
		written = int(r0)
		return errno != syscall.EAGAIN && errno != syscall.EINTR
	})
	if err != nil {
		return 0, err
	}
	if errno != 0 {
		return 0, errno
	}
	return written, nil
}

// groHdrScratch returns one header slot per buffer, growing the store if a
// larger batch shows up. It is guarded by writeOpMu.
func (tun *NativeTun) groHdrScratch(n int) [][]byte {
	if len(tun.gsoHdrs) < n {
		tun.gsoHdrs = make([][virtioNetHdrTnlLen]byte, n)
		tun.gsoHdrViews = make([][]byte, n)
		for i := range tun.gsoHdrs {
			tun.gsoHdrViews[i] = tun.gsoHdrs[i][:]
		}
	}
	return tun.gsoHdrViews[:n]
}

// TunnelGROStats reports why encapsulated packets were, or were not, coalesced
// as tunnel GSO.
func (tun *NativeTun) TunnelGROStats() TunnelGROStats {
	tun.writeOpMu.Lock()
	defer tun.writeOpMu.Unlock()
	if tun.tunnelGROTable == nil {
		return TunnelGROStats{}
	}
	return tun.tunnelGROTable.stats
}

// handleVirtioRead splits in into bufs, leaving offset bytes at the front of
// each buffer. It mutates sizes to reflect the size of each element of bufs,
// and returns the number of packets read. hdrSize is the size of the virtio net
// header prefixing in, which is larger than virtioNetHdrLen once the UDP tunnel
// offloads of the Device are enabled.
func handleVirtioRead(in []byte, bufs [][]byte, sizes []int, offset int, hdrSize int) (int, error) {
	if hdrSize < virtioNetHdrLen || len(in) < hdrSize {
		return 0, io.ErrShortBuffer
	}
	var hdr virtioNetHdr
	err := hdr.decode(in)
	if err != nil {
		return 0, err
	}
	// The additional fields describing encapsulation only exist in the extended
	// virtio net header, so read them while the header is still in front of us.
	var tnl virtioNetHdrTnl
	tnl.virtioNetHdr = hdr
	if hdrSize >= virtioNetHdrTnlLen {
		if err := tnl.decode(in); err != nil {
			return 0, err
		}
	}
	in = in[hdrSize:]
	if hdr.gsoType == unix.VIRTIO_NET_HDR_GSO_NONE {
		if hdr.flags&unix.VIRTIO_NET_HDR_F_NEEDS_CSUM != 0 {
			// This means CHECKSUM_PARTIAL in skb context. We are responsible
			// for computing the checksum starting at hdr.csumStart and placing
			// at hdr.csumOffset.
			err = gsoNoneChecksum(in, hdr.csumStart, hdr.csumOffset)
			if err != nil {
				return 0, err
			}
		}
		if len(in) > len(bufs[0][offset:]) {
			return 0, fmt.Errorf("read len %d overflows bufs element len %d", len(in), len(bufs[0][offset:]))
		}
		n := copy(bufs[0][offset:], in)
		sizes[0] = n
		return 1, nil
	}
	if hdr.gsoType&virtioNetHdrGSOUDPTunnel != 0 {
		return handleVirtioTnlRead(in, tnl, bufs, sizes, offset)
	}
	if hdr.gsoType != unix.VIRTIO_NET_HDR_GSO_TCPV4 && hdr.gsoType != unix.VIRTIO_NET_HDR_GSO_TCPV6 && hdr.gsoType != unix.VIRTIO_NET_HDR_GSO_UDP_L4 {
		return 0, fmt.Errorf("unsupported virtio GSO type: %d", hdr.gsoType)
	}
	if len(in) == 0 {
		return 0, fmt.Errorf("packet is too short")
	}

	ipVersion := in[0] >> 4
	switch ipVersion {
	case 4:
		if hdr.gsoType != unix.VIRTIO_NET_HDR_GSO_TCPV4 && hdr.gsoType != unix.VIRTIO_NET_HDR_GSO_UDP_L4 {
			return 0, fmt.Errorf("ip header version: %d, GSO type: %d", ipVersion, hdr.gsoType)
		}
	case 6:
		if hdr.gsoType != unix.VIRTIO_NET_HDR_GSO_TCPV6 && hdr.gsoType != unix.VIRTIO_NET_HDR_GSO_UDP_L4 {
			return 0, fmt.Errorf("ip header version: %d, GSO type: %d", ipVersion, hdr.gsoType)
		}
	default:
		return 0, fmt.Errorf("invalid ip header version: %d", ipVersion)
	}

	// csumStart points at the transport header, which cannot precede the IP
	// header whose checksum gsoSplit clears.
	minCsumStart := uint16(ipv4HeaderLen)
	if ipVersion == 6 {
		minCsumStart = ipv6HeaderLen
	}
	if hdr.csumStart < minCsumStart {
		return 0, fmt.Errorf("virtioNetHdr.csumStart (%d) precedes the ip header (%d)", hdr.csumStart, minCsumStart)
	}

	// Don't trust hdr.hdrLen from the kernel as it can be equal to the length
	// of the entire first packet when the kernel is handling it as part of a
	// FORWARD path. Instead, parse the transport header length and add it onto
	// csumStart, which is synonymous for IP header length.
	if hdr.gsoType == unix.VIRTIO_NET_HDR_GSO_UDP_L4 {
		hdr.hdrLen = hdr.csumStart + 8
	} else {
		if len(in) <= int(hdr.csumStart)+12 {
			return 0, errors.New("packet is too short")
		}

		tcpHLen := uint16(in[int(hdr.csumStart)+12] >> 4 * 4)
		if tcpHLen < 20 || tcpHLen > 60 {
			// A TCP header must be between 20 and 60 bytes in length.
			return 0, fmt.Errorf("tcp header len is invalid: %d", tcpHLen)
		}
		hdr.hdrLen = hdr.csumStart + tcpHLen
	}

	if len(in) < int(hdr.hdrLen) {
		return 0, fmt.Errorf("length of packet (%d) < virtioNetHdr.hdrLen (%d)", len(in), hdr.hdrLen)
	}

	if hdr.hdrLen < hdr.csumStart {
		return 0, fmt.Errorf("virtioNetHdr.hdrLen (%d) < virtioNetHdr.csumStart (%d)", hdr.hdrLen, hdr.csumStart)
	}
	cSumAt := int(hdr.csumStart) + int(hdr.csumOffset)
	if cSumAt+1 >= len(in) {
		return 0, fmt.Errorf("end of checksum offset (%d) exceeds packet length (%d)", cSumAt+1, len(in))
	}

	protocol := uint8(unix.IPPROTO_UDP)
	if hdr.gsoType != unix.VIRTIO_NET_HDR_GSO_UDP_L4 {
		protocol = unix.IPPROTO_TCP
	}
	// A packet that is not encapsulated has no encapsulating headers to speak
	// of: the IP header whose length GSO adjusts is the innermost one too.
	l := gsoLayout{
		outerIphAt:   0,
		outerIphLen:  int(hdr.csumStart),
		outerIsV6:    ipVersion == 6,
		outerUdpAt:   -1,
		innerIphAt:   0,
		innerIphLen:  int(hdr.csumStart),
		innerIsV6:    ipVersion == 6,
		transportAt:  int(hdr.csumStart),
		transportLen: int(hdr.hdrLen) - int(hdr.csumStart),
		csumOffset:   int(hdr.csumOffset),
		protocol:     protocol,
	}
	return gsoSplit(in, hdr, l, bufs, sizes, offset)
}

// handleVirtioTnlRead splits an encapsulated GSO packet -- a GSO packet whose
// payload is itself encapsulated by a UDP tunnel -- as described by the
// extended virtio net header tnl, into bufs, leaving offset bytes at the front
// of each buffer. It mutates sizes to reflect the size of each element of bufs,
// and returns the number of packets read.
func handleVirtioTnlRead(in []byte, tnl virtioNetHdrTnl, bufs [][]byte, sizes []int, offset int) (int, error) {
	hdr := tnl.virtioNetHdr
	var l gsoLayout

	tunnelType := hdr.gsoType & virtioNetHdrGSOUDPTunnel
	if tunnelType == virtioNetHdrGSOUDPTunnel {
		return 0, errors.New("virtio GSO tunnel type is IPv4 and IPv6 at once")
	}
	l.outerIsV6 = tunnelType == virtioNetHdrGSOUDPTunnelIPv6

	// The UDP tunnel must carry a GSO packet, and the kernel only ever
	// describes one whose transport checksum is ours to compute.
	innerType := hdr.gsoType &^ (unix.VIRTIO_NET_HDR_GSO_ECN | virtioNetHdrGSOUDPTunnel)
	switch innerType {
	case unix.VIRTIO_NET_HDR_GSO_TCPV4, unix.VIRTIO_NET_HDR_GSO_TCPV6:
		l.protocol = unix.IPPROTO_TCP
	case unix.VIRTIO_NET_HDR_GSO_UDP_L4:
		l.protocol = unix.IPPROTO_UDP
	default:
		return 0, fmt.Errorf("unsupported virtio GSO type %#x inside tunnel", innerType)
	}
	if hdr.flags&unix.VIRTIO_NET_HDR_F_NEEDS_CSUM == 0 {
		return 0, errors.New("tunnel GSO without VIRTIO_NET_HDR_F_NEEDS_CSUM")
	}
	l.outerCSum = hdr.flags&virtioNetHdrFUDPTunnelCSum != 0
	if hdr.gsoSize == 0 {
		return 0, errors.New("virtioNetHdr.gsoSize is zero")
	}

	// Outer IP header.
	if len(in) < ipv4HeaderLen {
		return 0, errors.New("packet is too short")
	}
	if in[0]>>4 == 6 {
		if !l.outerIsV6 {
			return 0, fmt.Errorf("ip header version: %d, tunnel type: %#x", in[0]>>4, tunnelType)
		}
		l.outerIphLen = ipv6HeaderLen
	} else {
		if l.outerIsV6 {
			return 0, fmt.Errorf("ip header version: %d, tunnel type: %#x", in[0]>>4, tunnelType)
		}
		l.outerIphLen = int(in[0]&0x0F) * 4
		if l.outerIphLen < ipv4HeaderLen {
			return 0, fmt.Errorf("ipv4 header len is invalid: %d", l.outerIphLen)
		}
	}

	// Encapsulating UDP header, which the kernel points us at.
	l.outerUdpAt = int(tnl.outerThOffset)
	if l.outerUdpAt < l.outerIphLen || l.outerUdpAt+udphLen > len(in) {
		return 0, fmt.Errorf("virtioNetHdr.outerThOffset (%d) is out of bounds", tnl.outerThOffset)
	}
	if l.outerUdpAt != l.outerIphLen && l.outerUdpAt-tunTunnelOuterThBias == l.outerIphLen {
		// The bound the kernel enforces on this field counts an Ethernet
		// header that IFF_TUN never delivers, so a header written for it
		// carries the bias tunTunnelOuterThBias describes. Undo it here; the
		// position the outer UDP header actually sits at is the one the IP
		// header's own length implies.
		l.outerUdpAt -= tunTunnelOuterThBias
	}

	// Inner IP header, which the kernel points us at too.
	l.innerIphAt = int(tnl.innerNhOffset)
	if l.innerIphAt < l.outerUdpAt+udphLen || l.innerIphAt+ipv4HeaderLen > len(in) {
		return 0, fmt.Errorf("virtioNetHdr.innerNhOffset (%d) is out of bounds", tnl.innerNhOffset)
	}
	l.innerIsV6 = in[l.innerIphAt]>>4 == 6
	switch innerType {
	case unix.VIRTIO_NET_HDR_GSO_TCPV4:
		if l.innerIsV6 {
			return 0, errors.New("inner ip header version: 6, GSO type: TCPV4")
		}
	case unix.VIRTIO_NET_HDR_GSO_TCPV6:
		if !l.innerIsV6 {
			return 0, errors.New("inner ip header version: 4, GSO type: TCPV6")
		}
	}
	if l.innerIsV6 {
		l.innerIphLen = ipv6HeaderLen
	} else {
		l.innerIphLen = int(in[l.innerIphAt]&0x0F) * 4
		if l.innerIphLen < ipv4HeaderLen {
			return 0, fmt.Errorf("inner ipv4 header len is invalid: %d", l.innerIphLen)
		}
	}

	// The innermost transport header is where the kernel places the checksum
	// that GSO offload leaves for us to compute.
	l.transportAt = int(hdr.csumStart)
	l.csumOffset = int(hdr.csumOffset)
	if l.transportAt < l.innerIphAt+l.innerIphLen {
		return 0, fmt.Errorf("virtioNetHdr.csumStart (%d) precedes inner ip header", hdr.csumStart)
	}
	transportLen, err := transportHeaderLen(in, l.transportAt, l.protocol)
	if err != nil {
		return 0, err
	}
	l.transportLen = transportLen

	// Don't trust hdr.hdrLen from the kernel, for the same reason as above: it
	// is only a hint at how much of the packet is linear.
	hdr.hdrLen = uint16(l.transportAt + l.transportLen)
	if int(hdr.hdrLen) > len(in) {
		return 0, fmt.Errorf("length of packet (%d) < virtioNetHdr.hdrLen (%d)", len(in), hdr.hdrLen)
	}
	cSumAt := l.transportAt + l.csumOffset
	if cSumAt+1 >= len(in) {
		return 0, fmt.Errorf("end of checksum offset (%d) exceeds packet length (%d)", cSumAt+1, len(in))
	}

	return gsoSplit(in, hdr, l, bufs, sizes, offset)
}

func (tun *NativeTun) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	tun.readOpMu.Lock()
	defer tun.readOpMu.Unlock()
	select {
	case err := <-tun.errors:
		return 0, err
	default:
		readInto := bufs[0][offset:]
		if tun.vnetHdr {
			readInto = tun.readBuff[:tun.vnetHdrSize+maxPktLen]
		}
		n, err := tun.tunFile.Read(readInto)
		if errors.Is(err, syscall.EBADFD) {
			err = os.ErrClosed
		}
		if err != nil {
			return 0, err
		}
		if tun.vnetHdr {
			return handleVirtioRead(readInto[:n], bufs, sizes, offset, tun.vnetHdrSize)
		} else {
			sizes[0] = n
			return 1, nil
		}
	}
}

func (tun *NativeTun) Events() <-chan Event {
	return tun.events
}

func (tun *NativeTun) Close() error {
	var err1, err2 error
	tun.closeOnce.Do(func() {
		if tun.statusListenersShutdown != nil {
			close(tun.statusListenersShutdown)
			if tun.netlinkCancel != nil {
				err1 = tun.netlinkCancel.Cancel()
			}
		} else if tun.events != nil {
			close(tun.events)
		}
		err2 = tun.tunFile.Close()
	})
	if err1 != nil {
		return err1
	}
	return err2
}

func (tun *NativeTun) BatchSize() int {
	return tun.batchSize
}

const (
	// TODO: support TSO with ECN bits
	tunTCPOffloads = unix.TUN_F_CSUM | unix.TUN_F_TSO4 | unix.TUN_F_TSO6
	tunUDPOffloads = unix.TUN_F_USO4 | unix.TUN_F_USO6
	// These are defined in the kernel in include/uapi/linux/if_tun.h, where
	// golang.org/x/sys/unix has not caught up with them yet. They let the kernel
	// hand us an entire GSO packet that encapsulates another GSO packet in a UDP
	// tunnel, such as VxLAN, rather than segmenting it into MTU sized packets
	// for us first.
	tunUDPTunnelGSO     = 0x80  // TUN_F_UDP_TUNNEL_GSO
	tunUDPTunnelGSOCSum = 0x100 // TUN_F_UDP_TUNNEL_GSO_CSUM
)

// setVnetHdrLen sets the size of the virtio net header with which the kernel
// prefixes every packet, and reads it back to be sure the kernel agreed.
func setVnetHdrLen(fd, size int) error {
	if err := unix.IoctlSetPointerInt(fd, unix.TUNSETVNETHDRSZ, size); err != nil {
		return err
	}
	got, err := unix.IoctlGetInt(fd, unix.TUNGETVNETHDRSZ)
	if err != nil {
		return err
	}
	if got != size {
		return fmt.Errorf("TUNGETVNETHDRSZ reports %d rather than %d", got, size)
	}
	return nil
}

func (tun *NativeTun) initFromFlags(name string) error {
	sc, err := tun.tunFile.SyscallConn()
	if err != nil {
		return err
	}
	if e := sc.Control(func(fd uintptr) {
		var (
			ifr *unix.Ifreq
		)
		ifr, err = unix.NewIfreq(name)
		if err != nil {
			return
		}
		err = unix.IoctlIfreq(int(fd), unix.TUNGETIFF, ifr)
		if err != nil {
			return
		}
		got := ifr.Uint16()
		if got&unix.IFF_VNET_HDR != 0 {
			// tunTCPOffloads were added in Linux v2.6. We require their support
			// if IFF_VNET_HDR is set.
			offloads := int(tunTCPOffloads)
			err = unix.IoctlSetInt(int(fd), unix.TUNSETOFFLOAD, offloads)
			if err != nil {
				return
			}
			tun.vnetHdr = true
			tun.vnetHdrSize = virtioNetHdrLen
			tun.batchSize = conn.IdealBatchSize
			// tunUDPOffloads were added in Linux v6.2. We do not return an
			// error if they are unsupported at runtime.
			if unix.IoctlSetInt(int(fd), unix.TUNSETOFFLOAD, offloads|tunUDPOffloads) == nil {
				tun.udpGSO = true
				offloads |= tunUDPOffloads
			}
			// tunUDPTunnelGSO were added in Linux v6.17. We do not return an
			// error if they are unsupported at runtime. The kernel grows the
			// virtio net header along with them, so they are only of use if the
			// header size change takes too.
			if unix.IoctlSetInt(int(fd), unix.TUNSETOFFLOAD, offloads|tunUDPTunnelGSO|tunUDPTunnelGSOCSum) == nil {
				if setVnetHdrLen(int(fd), virtioNetHdrTnlLen) == nil {
					tun.vnetHdrSize = virtioNetHdrTnlLen
				} else {
					// Without the larger header the kernel cannot describe an
					// encapsulated GSO packet to us at all, so undo the offloads.
					_ = unix.IoctlSetInt(int(fd), unix.TUNSETOFFLOAD, offloads)
				}
			}
		} else {
			tun.batchSize = 1
		}
	}); e != nil {
		return e
	}
	return err
}

// CreateTUN creates a Device with the provided name and MTU.
func CreateTUN(name string, mtu int) (Device, error) {
	nfd, err := unix.Open(cloneDevicePath, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("CreateTUN(%q) failed; %s does not exist", name, cloneDevicePath)
		}
		return nil, err
	}

	ifr, err := unix.NewIfreq(name)
	if err != nil {
		return nil, err
	}
	// IFF_VNET_HDR enables the "tun status hack" via routineHackListener()
	// where a null write will return EINVAL indicating the TUN is up.
	ifr.SetUint16(unix.IFF_TUN | unix.IFF_NO_PI | unix.IFF_VNET_HDR)
	err = unix.IoctlIfreq(nfd, unix.TUNSETIFF, ifr)
	if err != nil {
		return nil, err
	}

	err = unix.SetNonblock(nfd, true)
	if err != nil {
		unix.Close(nfd)
		return nil, err
	}

	// Note that the above -- open,ioctl,nonblock -- must happen prior to handing it to netpoll as below this line.

	fd := os.NewFile(uintptr(nfd), cloneDevicePath)
	return CreateTUNFromFile(fd, mtu)
}

// CreateTUNFromFile creates a Device from an os.File with the provided MTU.
func CreateTUNFromFile(file *os.File, mtu int) (Device, error) {
	tun := &NativeTun{
		tunFile:                 file,
		events:                  make(chan Event, 5),
		errors:                  make(chan error, 5),
		statusListenersShutdown: make(chan struct{}),
		vnetHdrSize:             virtioNetHdrLen,
		tcpGROTable:             newTCPGROTable(),
		udpGROTable:             newUDPGROTable(),
		tunnelGROTable:          newTunnelGROTable(),
		toWrite:                 make([]int, 0, conn.IdealBatchSize),
	}

	name, err := tun.Name()
	if err != nil {
		return nil, err
	}

	err = tun.initFromFlags(name)
	if err != nil {
		return nil, err
	}

	// start event listener
	tun.index, err = getIFIndex(name)
	if err != nil {
		return nil, err
	}

	tun.netlinkSock, err = createNetlinkSocket()
	if err != nil {
		return nil, err
	}
	tun.netlinkCancel, err = rwcancel.NewRWCancel(tun.netlinkSock)
	if err != nil {
		unix.Close(tun.netlinkSock)
		return nil, err
	}

	tun.hackListenerClosed.Lock()
	go tun.routineNetlinkListener()
	go tun.routineHackListener() // cross namespace

	err = tun.setMTU(mtu)
	if err != nil {
		unix.Close(tun.netlinkSock)
		return nil, err
	}

	return tun, nil
}

// CreateUnmonitoredTUNFromFD creates a Device from the provided file
// descriptor.
func CreateUnmonitoredTUNFromFD(fd int) (Device, string, error) {
	err := unix.SetNonblock(fd, true)
	if err != nil {
		return nil, "", err
	}
	file := os.NewFile(uintptr(fd), "/dev/tun")
	tun := &NativeTun{
		tunFile:        file,
		events:         make(chan Event, 5),
		errors:         make(chan error, 5),
		vnetHdrSize:    virtioNetHdrLen,
		tcpGROTable:    newTCPGROTable(),
		udpGROTable:    newUDPGROTable(),
		tunnelGROTable: newTunnelGROTable(),
		toWrite:        make([]int, 0, conn.IdealBatchSize),
	}
	name, err := tun.Name()
	if err != nil {
		return nil, "", err
	}
	err = tun.initFromFlags(name)
	if err != nil {
		return nil, "", err
	}
	return tun, name, err
}
