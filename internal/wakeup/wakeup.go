// Package wakeup delivers the Wi-Fi Direct credentials to the companion over
// an IP link instead of Bluetooth.
//
// The CMF Phone 2 Pro's controller accepts a BLE advertisement and then never
// puts it on air, so the laptop cannot discover the phone that way. Tailscale
// already reaches the phone, and that link only has to carry one small
// message: the group SSID, passphrase and channel. The screen mirror itself
// still runs over Wi-Fi Direct, which is the fast local radio.
package wakeup

import (
	"context"
	"fmt"
	"net"
	"time"

	"breadway.dev/breadpair/internal/protocol"
)

// Port is where the companion listens. High and unprivileged, and not a port
// anything else in the bread stack uses.
const Port = 45550

// Send dials the companion and writes one credential. The companion closes
// after reading it, so a clean EOF is success.
func Send(ctx context.Context, addr string, cred protocol.Credential) error {
	payload, err := cred.Encode()
	if err != nil {
		return err
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("reach the companion at %s: %w", addr, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write(payload); err != nil {
		return fmt.Errorf("send credentials: %w", err)
	}
	return nil
}
