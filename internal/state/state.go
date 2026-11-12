// Package state persists the live session so `phone down` and `phone status`
// work from a new process. The file is mode 600 because it holds the group
// passphrase.
package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// File is the on-disk session.
const File = "session.json"

// Session mirrors the fields `down` needs. It is deliberately a flat struct
// rather than the live wifi.Session, so the state package does not import the
// code that creates processes.
type Session struct {
	SupplicantPID int    `json:"supplicant_pid"`
	ConfPath      string `json:"conf_path"`
	LogPath       string `json:"log_path"`
	StationIface  string `json:"station_iface"`
	OwnedWLAN     bool   `json:"owned_wlan"`

	GroupIface string `json:"group_iface,omitempty"`
	SSID       string `json:"ssid,omitempty"`
	Passphrase string `json:"passphrase,omitempty"`
	Frequency  int    `json:"frequency,omitempty"`
	GroupMAC   string `json:"group_mac,omitempty"`

	StationSSID   string `json:"station_ssid,omitempty"`
	StationIP     string `json:"station_ip,omitempty"`
	StationRouter string `json:"station_router,omitempty"`
	StationMetric int    `json:"station_metric,omitempty"`
	StationRoute  bool   `json:"station_route,omitempty"`

	DHCPPid  int  `json:"dhcp_pid,omitempty"`
	UFWAdded bool `json:"ufw_added,omitempty"`

	ScrcpyPID int    `json:"scrcpy_pid,omitempty"`
	ADBSerial string `json:"adb_serial,omitempty"`
	PhoneIP   string `json:"phone_ip,omitempty"`
}

// Dir is ~/.local/state/breadpair, created on demand.
func Dir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".local", "state", "breadpair")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// Save writes the session atomically.
func Save(dir string, s Session) error {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp := filepath.Join(dir, File+".tmp")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, File))
}

// Load reads the session. A missing file is os.ErrNotExist.
func Load(dir string) (Session, error) {
	raw, err := os.ReadFile(filepath.Join(dir, File))
	if err != nil {
		return Session{}, err
	}
	var s Session
	if err := json.Unmarshal(raw, &s); err != nil {
		return Session{}, err
	}
	return s, nil
}

// Clear removes the session file. Missing is fine.
func Clear(dir string) error {
	err := os.Remove(filepath.Join(dir, File))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
