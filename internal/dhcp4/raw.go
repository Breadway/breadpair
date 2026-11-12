package dhcp4

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"time"

	"golang.org/x/sys/unix"
)

// acquireRaw broadcasts DHCP on iface through a packet socket. Binding UDP
// port 68 is not possible here: NetworkManager holds that port for the
// ethernet lease, and the port is one per host, not one per interface.
func acquireRaw(ctx context.Context, iface string) (Lease, error) {
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return Lease{}, err
	}
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_DGRAM, int(htons(unix.ETH_P_IP)))
	if err != nil {
		return Lease{}, fmt.Errorf("packet socket: %w", err)
	}
	defer unix.Close(fd)

	addr := &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_IP), Ifindex: ifi.Index}
	if err := unix.Bind(fd, addr); err != nil {
		return Lease{}, fmt.Errorf("bind packet socket to %s: %w", iface, err)
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(10 * time.Second)
	}
	tv := unix.NsecToTimeval(time.Second.Nanoseconds())
	_ = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv)

	var xid [4]byte
	_, _ = randRead(xid[:])
	mac := ifi.HardwareAddr

	offer, err := rawExchange(ctx, fd, ifi.Index, mac, xid, discover(xid, mac), msgOffer, deadline)
	if err != nil {
		return Lease{}, fmt.Errorf("discover: %w", err)
	}
	ack, err := rawExchange(ctx, fd, ifi.Index, mac, xid, request(xid, mac, offer), msgAck, deadline)
	if err != nil {
		return Lease{}, fmt.Errorf("request: %w", err)
	}
	return ack.lease, nil
}

func rawExchange(ctx context.Context, fd, ifindex int, mac net.HardwareAddr, xid [4]byte, payload []byte, want byte, deadline time.Time) (reply, error) {
	frame := udpBroadcast(payload)
	dst := &unix.SockaddrLinklayer{
		Protocol: htons(unix.ETH_P_IP),
		Ifindex:  ifindex,
		Halen:    6,
	}
	for i := 0; i < 6; i++ {
		dst.Addr[i] = 0xff
	}
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return reply{}, err
		}
		if err := unix.Sendto(fd, frame, 0, dst); err != nil {
			return reply{}, err
		}
		until := time.Now().Add(2 * time.Second)
		for time.Now().Before(until) {
			buf := make([]byte, 1500)
			n, _, err := unix.Recvfrom(fd, buf, 0)
			if err != nil {
				break
			}
			udp, ok := unwrapUDP(buf[:n])
			if !ok {
				continue
			}
			got, ok := parseReply(udp, mac, xid)
			if !ok {
				continue
			}
			// A late OFFER for this xid can still be in flight when the
			// REQUEST goes out. Skip anything that is not the step we are on.
			if got.kind != want {
				if got.kind == msgNak {
					return reply{}, fmt.Errorf("server refused the request")
				}
				continue
			}
			return got, nil
		}
	}
	return reply{}, fmt.Errorf("no reply")
}

// udpBroadcast wraps a DHCP payload in an IPv4 and UDP header aimed at
// 255.255.255.255:67 from 0.0.0.0:68.
func udpBroadcast(payload []byte) []byte {
	udpLen := 8 + len(payload)
	total := 20 + udpLen
	pkt := make([]byte, total)
	pkt[0] = 0x45
	binary.BigEndian.PutUint16(pkt[2:4], uint16(total))
	pkt[8] = 64
	pkt[9] = unix.IPPROTO_UDP
	copy(pkt[12:16], net.IPv4zero.To4())
	copy(pkt[16:20], net.IPv4bcast.To4())
	binary.BigEndian.PutUint16(pkt[10:12], ipChecksum(pkt[:20]))
	binary.BigEndian.PutUint16(pkt[20:22], 68)
	binary.BigEndian.PutUint16(pkt[22:24], 67)
	binary.BigEndian.PutUint16(pkt[24:26], uint16(udpLen))
	copy(pkt[28:], payload)
	return pkt
}

func unwrapUDP(pkt []byte) ([]byte, bool) {
	if len(pkt) < 28 || pkt[0]>>4 != 4 {
		return nil, false
	}
	ihl := int(pkt[0]&0x0f) * 4
	if len(pkt) < ihl+8 || pkt[9] != unix.IPPROTO_UDP {
		return nil, false
	}
	dst := binary.BigEndian.Uint16(pkt[ihl+2 : ihl+4])
	if dst != 68 {
		return nil, false
	}
	return pkt[ihl+8:], true
}

func ipChecksum(h []byte) uint16 {
	var sum uint32
	for i := 0; i < len(h); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(h[i : i+2]))
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}

func htons(v uint16) uint16 {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, v)
	return binary.NativeEndian.Uint16(b)
}
