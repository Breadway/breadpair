// Package protocol is the byte layout shared by the laptop and the Android
// companion. Both sides speak it, so the constants here are the contract.
//
// The companion advertises a manufacturer payload (Bluetooth company id is
// the prototype range 0xFFFF) and a GATT service. The laptop scans for the
// service UUID, reads the payload, and writes the Wi-Fi Direct credentials
// back to the control characteristic. The phone then joins the laptop's group.
package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
)

// ServiceUUID is the 128-bit UUID the companion exposes over GATT. Picked at
// random; it is not a registered Bluetooth SIG UUID.
const ServiceUUID = "8a6e1b10-7c4d-4e2a-9f31-b6d0a4c85e17"

// ServiceAlias is the 16-bit UUID the companion puts in the advertisement
// itself. A legacy advertisement is 31 bytes and the 128-bit UUID does not fit
// next to the manufacturer payload, so the alias is what a scanner matches
// and the full UUID is served in the scan response.
const ServiceAlias = "1b10"

// ControlUUID is the GATT characteristic the laptop writes the group
// credentials to. Write Without Response, so it works while the phone is
// only connected for the duration of one write.
const ControlUUID = "8a6e1b11-7c4d-4e2a-9f31-b6d0a4c85e17"

// StatusUUID is the GATT characteristic the phone updates with its adb
// port once wireless debugging is listening. Read + Notify.
const StatusUUID = "8a6e1b12-7c4d-4e2a-9f31-b6d0a4c85e17"

// CompanyID is the manufacturer id in front of the advertisement payload.
// 0xFFFF is the Bluetooth SIG "internal use" range, which is correct for a
// prototype that has no company id.
const CompanyID = 0xFFFF

// Magic is the first two bytes of every breadpair payload, ASCII "BP".
var Magic = [2]byte{'B', 'P'}

// Version is the payload layout version. Bump it when the bytes change.
const Version = 1

// Advertisement flags.
const (
	FlagArmed     = 1 << 0 // the companion will join a group
	FlagScreenOff = 1 << 1 // the phone reported its screen as off
	FlagADBReady  = 1 << 2 // wireless debugging is listening
)

// Payload is what the companion puts in its manufacturer data.
type Payload struct {
	Version   byte
	Flags     byte
	ADBPort   uint16  // 0 when wireless debugging is not listening
	P2PDevice [6]byte // phone Wi-Fi Direct device address, zeros if unknown
}

// Encode renders the payload. Layout, little endian:
//
//	0..1  magic "BP"
//	2     version
//	3     flags
//	4..5  adb port
//	6..11 P2P device address
func (p Payload) Encode() []byte {
	out := make([]byte, 12)
	copy(out[0:2], Magic[:])
	out[2] = p.Version
	out[3] = p.Flags
	binary.LittleEndian.PutUint16(out[4:6], p.ADBPort)
	copy(out[6:12], p.P2PDevice[:])
	return out
}

// Decode parses a manufacturer payload. The leading company id is stripped
// by BlueZ before we see it, so this is the value bytes only.
func Decode(b []byte) (Payload, error) {
	if len(b) < 12 {
		return Payload{}, fmt.Errorf("payload is %d bytes, want 12", len(b))
	}
	if b[0] != Magic[0] || b[1] != Magic[1] {
		return Payload{}, errors.New("payload magic is not BP")
	}
	if b[2] != Version {
		return Payload{}, fmt.Errorf("payload version %d, this build speaks %d", b[2], Version)
	}
	var p Payload
	p.Version = b[2]
	p.Flags = b[3]
	p.ADBPort = binary.LittleEndian.Uint16(b[4:6])
	copy(p.P2PDevice[:], b[6:12])
	return p, nil
}

// P2PAddress renders the device address the way wpa_supplicant prints it.
func (p Payload) P2PAddress() string {
	if p.P2PDevice == [6]byte{} {
		return ""
	}
	return net.HardwareAddr(p.P2PDevice[:]).String()
}

// Credential is what the laptop writes to ControlUUID once the group is up.
// Layout:
//
//	0    version
//	1    ssid length
//	2..  ssid (up to 32 bytes)
//	..   passphrase length
//	..   passphrase (8 to 63 bytes)
//	..   frequency, uint16 little endian (MHz)
type Credential struct {
	SSID       string
	Passphrase string
	Frequency  int
}

// Encode renders a credential for the control characteristic.
func (c Credential) Encode() ([]byte, error) {
	if c.SSID == "" || len(c.SSID) > 32 {
		return nil, fmt.Errorf("ssid %q is empty or longer than 32 bytes", c.SSID)
	}
	if len(c.Passphrase) < 8 || len(c.Passphrase) > 63 {
		return nil, fmt.Errorf("passphrase must be 8 to 63 bytes, got %d", len(c.Passphrase))
	}
	if c.Frequency < 2412 || c.Frequency > 5825 {
		return nil, fmt.Errorf("frequency %d is outside the 2.4/5 GHz range", c.Frequency)
	}
	out := make([]byte, 0, 2+len(c.SSID)+1+len(c.Passphrase)+2)
	out = append(out, Version, byte(len(c.SSID)))
	out = append(out, c.SSID...)
	out = append(out, byte(len(c.Passphrase)))
	out = append(out, c.Passphrase...)
	var freq [2]byte
	binary.LittleEndian.PutUint16(freq[:], uint16(c.Frequency))
	out = append(out, freq[:]...)
	return out, nil
}

// DecodeCredential parses a control-characteristic write.
func DecodeCredential(b []byte) (Credential, error) {
	if len(b) < 4 {
		return Credential{}, errors.New("credential is too short")
	}
	if b[0] != Version {
		return Credential{}, fmt.Errorf("credential version %d, this build speaks %d", b[0], Version)
	}
	nSSID := int(b[1])
	if len(b) < 2+nSSID+1 {
		return Credential{}, errors.New("credential truncated in the ssid")
	}
	ssid := string(b[2 : 2+nSSID])
	rest := b[2+nSSID:]
	nPass := int(rest[0])
	if len(rest) < 1+nPass+2 {
		return Credential{}, errors.New("credential truncated in the passphrase")
	}
	pass := string(rest[1 : 1+nPass])
	freq := binary.LittleEndian.Uint16(rest[1+nPass : 3+nPass])
	return Credential{SSID: ssid, Passphrase: pass, Frequency: int(freq)}, nil
}
