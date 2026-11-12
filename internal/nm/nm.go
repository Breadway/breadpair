// Package nm reads the station side of NetworkManager. breadpair never takes
// wlan0 away from it: the whole point of the dedicated P2P interface is that
// the normal Wi-Fi connection stays up.
package nm

import (
	"context"
	"fmt"
	"strings"

	"breadway.dev/breadpair/internal/executil"
)

// Device is one row of `nmcli device status`.
type Device struct {
	Name  string
	Type  string
	State string
}

// Devices lists every device NetworkManager knows about.
func Devices(ctx context.Context) ([]Device, error) {
	out, err := executil.Output(ctx, "nmcli", "-t", "-f", "DEVICE,TYPE,STATE", "device", "status")
	if err != nil {
		return nil, fmt.Errorf("ask NetworkManager for devices: %w", err)
	}
	var devs []Device
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		devs = append(devs, Device{Name: parts[0], Type: parts[1], State: parts[2]})
	}
	return devs, nil
}

// Station is the managed Wi-Fi interface that carries normal traffic, if one
// is connected. An empty result means the machine is on ethernet or offline,
// which is fine: the P2P group does not need it.
type Station struct {
	Iface string
	SSID  string
}

// ConnectedStation returns the connected Wi-Fi station, or nil when there is
// none. A device that is "unmanaged" is reported as an error because that is
// the state the old manual flow left wlan0 in, and bringing a second
// supplicant up against it fights NetworkManager.
func ConnectedStation(ctx context.Context, iface string) (*Station, error) {
	devs, err := Devices(ctx)
	if err != nil {
		return nil, err
	}
	for _, d := range devs {
		if d.Name != iface || d.Type != "wifi" {
			continue
		}
		switch {
		case strings.HasPrefix(d.State, "connected"):
			ssid, err := executil.Output(ctx, "nmcli", "-t", "-f", "ACTIVE,SSID", "device", "wifi")
			if err != nil {
				return &Station{Iface: iface}, nil
			}
			for _, line := range strings.Split(ssid, "\n") {
				if strings.HasPrefix(line, "yes:") {
					return &Station{Iface: iface, SSID: strings.TrimPrefix(line, "yes:")}, nil
				}
			}
			return &Station{Iface: iface}, nil
		case strings.HasPrefix(d.State, "unmanaged"):
			return nil, fmt.Errorf("%s is unmanaged, which means another wpa_supplicant already owns it; "+
				"run 'phone down' or stop that supplicant before 'phone up', otherwise the normal Wi-Fi connection cannot come back", iface)
		default:
			return nil, nil
		}
	}
	return nil, nil
}

// DefaultRouteDev is the interface the kernel sends the default route through.
// Used by status to show that internet did not move onto the P2P interface.
func DefaultRouteDev(ctx context.Context) (string, error) {
	out, err := executil.Output(ctx, "ip", "-4", "route", "show", "default")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		for i, f := range fields {
			if f == "dev" && i+1 < len(fields) {
				return fields[i+1], nil
			}
		}
	}
	return "", nil
}
