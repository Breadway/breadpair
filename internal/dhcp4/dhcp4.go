// Package dhcp4 is a minimal DHCPv4 client. The laptop uses it twice: once to
// take an address for the station interface (so normal Wi-Fi keeps working
// while wpa_supplicant, not NetworkManager, owns it), and the state package
// watches dnsmasq leases for the phone. NetworkManager's DHCP client is
// internal and there is no dhcpcd or dhclient on this machine.
package dhcp4

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"time"
)

func randRead(b []byte) (int, error) { return rand.Read(b) }

const (
	bootRequest = 1
	bootReply   = 2

	optMask        = 1
	optRouter      = 3
	optDNS         = 6
	optRequestedIP = 50
	optLeaseTime   = 51
	optType        = 53
	optServerID    = 54
	optParamReq    = 55
	optEnd         = 255

	msgDiscover = 1
	msgOffer    = 2
	msgRequest  = 3
	msgAck      = 5
	msgNak      = 6
)

// Lease is a granted IPv4 configuration.
type Lease struct {
	IP      net.IP
	Mask    net.IPMask
	Router  net.IP
	DNS     []net.IP
	Server  net.IP
	Seconds uint32
}

func (l Lease) Prefix() int {
	ones, _ := l.Mask.Size()
	return ones
}

// Acquire broadcasts DISCOVER/REQUEST on iface and returns the ACK. xid is
// random per call. The client does not renew; breadpair sessions are short
// and `down` releases the address.
//
// The packet socket needs root. `phone dhcp4` runs under sudo for that.
func Acquire(ctx context.Context, iface string) (Lease, error) {
	return acquireRaw(ctx, iface)
}

// acquireUDP is the UDP implementation, kept for tests and for machines where
// port 68 is free. It is not used on the laptop: NetworkManager holds port 68
// for the ethernet lease.
func acquireUDP(ctx context.Context, iface string) (Lease, error) {
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return Lease{}, fmt.Errorf("interface %s: %w", iface, err)
	}
	if len(ifi.HardwareAddr) != 6 {
		return Lease{}, fmt.Errorf("%s has no ethernet address", iface)
	}

	conn, err := net.ListenPacket("udp4", ":68")
	if err != nil {
		return Lease{}, fmt.Errorf("bind dhcp client port: %w", err)
	}
	defer conn.Close()

	var xid [4]byte
	_, _ = rand.Read(xid[:])

	offer, err := exchange(ctx, conn, ifi.HardwareAddr, xid, discover(xid, ifi.HardwareAddr))
	if err != nil {
		return Lease{}, fmt.Errorf("dhcp discover on %s: %w", iface, err)
	}
	if offer.kind != msgOffer {
		return Lease{}, fmt.Errorf("dhcp server sent %s, want an offer", kindName(offer.kind))
	}
	ack, err := exchange(ctx, conn, ifi.HardwareAddr, xid, request(xid, ifi.HardwareAddr, offer))
	if err != nil {
		return Lease{}, fmt.Errorf("dhcp request on %s: %w", iface, err)
	}
	if ack.kind == msgNak {
		return Lease{}, fmt.Errorf("dhcp server refused the request")
	}
	if ack.kind != msgAck {
		return Lease{}, fmt.Errorf("dhcp server sent %s, want an ack", kindName(ack.kind))
	}
	return ack.lease, nil
}

type reply struct {
	kind  byte
	lease Lease
}

func exchange(ctx context.Context, conn net.PacketConn, mac net.HardwareAddr, xid [4]byte, pkt []byte) (reply, error) {
	dst := &net.UDPAddr{IP: net.IPv4bcast, Port: 67}
	deadline, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		deadline = time.Now().Add(10 * time.Second)
	}
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return reply{}, err
		}
		_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
		if _, err := conn.WriteTo(pkt, dst); err != nil {
			return reply{}, err
		}
		buf := make([]byte, 1500)
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		for {
			n, _, err := conn.ReadFrom(buf)
			if err != nil {
				break
			}
			got, ok := parseReply(buf[:n], mac, xid)
			if ok {
				return got, nil
			}
		}
	}
	return reply{}, fmt.Errorf("no reply before the deadline")
}

func discover(xid [4]byte, mac net.HardwareAddr) []byte {
	return packet(xid, mac, nil, append([]byte{optType, 1, msgDiscover}, paramReq()...))
}

func request(xid [4]byte, mac net.HardwareAddr, offer reply) []byte {
	opts := []byte{optType, 1, msgRequest}
	opts = append(opts, optRequestedIP, 4)
	opts = append(opts, offer.lease.IP.To4()...)
	opts = append(opts, optServerID, 4)
	opts = append(opts, offer.lease.Server.To4()...)
	opts = append(opts, paramReq()...)
	return packet(xid, mac, nil, opts)
}

func paramReq() []byte {
	return []byte{optParamReq, 3, optMask, optRouter, optDNS}
}

func packet(xid [4]byte, mac net.HardwareAddr, ciaddr net.IP, opts []byte) []byte {
	p := make([]byte, 240+len(opts)+1)
	p[0] = bootRequest
	p[1] = 1 // ethernet
	p[2] = 6
	copy(p[4:8], xid[:])
	if ciaddr != nil {
		copy(p[12:16], ciaddr.To4())
	}
	copy(p[28:34], mac)
	copy(p[236:240], []byte{99, 130, 83, 99})
	copy(p[240:], opts)
	p[len(p)-1] = optEnd
	return p
}

func parseReply(p []byte, mac net.HardwareAddr, xid [4]byte) (reply, bool) {
	if len(p) < 240 || p[0] != bootReply {
		return reply{}, false
	}
	if [4]byte{p[4], p[5], p[6], p[7]} != xid {
		return reply{}, false
	}
	if net.HardwareAddr(p[28:34]).String() != mac.String() {
		return reply{}, false
	}
	var r reply
	r.lease.IP = net.IP(append([]byte(nil), p[16:20]...))
	i := 240
	for i < len(p) {
		code := p[i]
		if code == optEnd {
			break
		}
		if code == 0 {
			i++
			continue
		}
		if i+1 >= len(p) {
			return reply{}, false
		}
		n := int(p[i+1])
		if i+2+n > len(p) {
			return reply{}, false
		}
		val := p[i+2 : i+2+n]
		switch code {
		case optType:
			if n == 1 {
				r.kind = val[0]
			}
		case optMask:
			if n == 4 {
				r.lease.Mask = net.IPMask(append([]byte(nil), val...))
			}
		case optRouter:
			if n >= 4 {
				r.lease.Router = net.IP(append([]byte(nil), val[:4]...))
			}
		case optDNS:
			for off := 0; off+4 <= n; off += 4 {
				r.lease.DNS = append(r.lease.DNS, net.IP(append([]byte(nil), val[off:off+4]...)))
			}
		case optServerID:
			if n == 4 {
				r.lease.Server = net.IP(append([]byte(nil), val...))
			}
		case optLeaseTime:
			if n == 4 {
				r.lease.Seconds = binary.BigEndian.Uint32(val)
			}
		}
		i += 2 + n
	}
	if r.kind == 0 || r.lease.IP == nil {
		return reply{}, false
	}
	return r, true
}

func kindName(k byte) string {
	switch k {
	case msgOffer:
		return "OFFER"
	case msgAck:
		return "ACK"
	case msgNak:
		return "NAK"
	default:
		return fmt.Sprintf("type %d", k)
	}
}
