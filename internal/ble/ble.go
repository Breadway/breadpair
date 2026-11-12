// Package ble scans for the breadpair companion through BlueZ.
//
// It uses bluetoothctl rather than a D-Bus library. bluetoothctl is part of
// bluez-utils, which is already required for the adapter, and its scan output
// is stable enough to parse. The filter is the companion service UUID plus the
// manufacturer payload defined in internal/protocol.
package ble

import (
	"bufio"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"breadway.dev/breadpair/internal/protocol"
)

// Hit is one advertisement that matched the companion.
type Hit struct {
	Address string // BLE address, usually a resolvable private address
	RSSI    int
	Name    string
	Payload protocol.Payload
	SeenAt  time.Time
}

func (h Hit) String() string {
	return fmt.Sprintf("%s rssi %d flags 0x%02x adb %d p2p %s",
		h.Address, h.RSSI, h.Payload.Flags, h.Payload.ADBPort, or(h.Payload.P2PAddress(), "unknown"))
}

func or(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// Push writes the group credentials to the companion's control characteristic.
// bluetoothctl drives the connection. The write is unencrypted on purpose:
// the group passphrase only has to cross the few seconds the phone is awake,
// and bonding would pop a dialog on a screen we are trying not to touch.
func Push(ctx context.Context, address string, cred protocol.Credential) error {
	raw, err := cred.Encode()
	if err != nil {
		return err
	}
	hexBytes := make([]string, len(raw))
	for i, b := range raw {
		hexBytes[i] = fmt.Sprintf("%02x", b)
	}
	script := fmt.Sprintf("connect %s\nmenu gatt\nselect-attribute %s\nwrite \"%s\"\nback\ndisconnect %s\nquit\n",
		address, protocol.ControlUUID, strings.Join(hexBytes, " "), address)

	cmd := exec.CommandContext(ctx, "bluetoothctl")
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	text := string(out)
	if err != nil {
		return fmt.Errorf("bluetoothctl: %w\n%s", err, trim(text))
	}
	if !strings.Contains(text, "WriteValue") && !strings.Contains(strings.ToLower(text), "success") {
		// bluetoothctl is chatty and does not always print a clean status.
		// A connection error is the case worth failing on.
		if strings.Contains(strings.ToLower(text), "failed") || strings.Contains(text, "not available") {
			return fmt.Errorf("could not write credentials to %s\n%s", address, trim(text))
		}
	}
	return nil
}

func trim(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 800 {
		return s[len(s)-800:]
	}
	return s
}

// Scan blocks until a companion advertisement arrives or ctx ends. adapter is
// the BlueZ adapter name, usually "hci0".
func Scan(ctx context.Context, adapter string, timeout time.Duration) (Hit, error) {
	scanCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(scanCtx, "bluetoothctl")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Hit{}, fmt.Errorf("bluetoothctl stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Hit{}, fmt.Errorf("bluetoothctl stdout: %w", err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return Hit{}, fmt.Errorf("start bluetoothctl: %w (is bluez running?)", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	// menu scan is the LE-only menu. transport le keeps the scan off the
	// classic inquiry, which is slow and irrelevant here.
	fmt.Fprintf(stdin, "menu scan\ntransport le\nduplicate-data on\nback\n")
	fmt.Fprintf(stdin, "select %s\nset-scan-filter-uuids %s\nscan on\n", adapter, protocol.ServiceUUID)

	hits := make(chan Hit, 1)
	errs := make(chan error, 1)
	go func() {
		h, err := readScan(scanCtx, stdout)
		if err != nil {
			errs <- err
			return
		}
		hits <- h
	}()

	select {
	case h := <-hits:
		fmt.Fprintln(stdin, "scan off")
		return h, nil
	case err := <-errs:
		return Hit{}, err
	case <-scanCtx.Done():
		fmt.Fprintln(stdin, "scan off")
		if ctx.Err() != nil {
			return Hit{}, ctx.Err()
		}
		return Hit{}, fmt.Errorf("no breadpair companion advertising %s after %s; "+
			"open the companion and tap Arm, or rerun with --skip-ble", protocol.ServiceUUID, timeout)
	}
}

// bluetoothctl prints one device per block. The lines we care about look like:
//
//	[CHG] Device AA:BB:CC:DD:EE:FF RSSI: -62
//	[CHG] Device AA:BB:CC:DD:EE:FF ManufacturerData.Value: 42 50 01 ...
//	[CHG] Device AA:BB:CC:DD:EE:FF ServiceData: ...
//
// Manufacturer data is delivered as a hex string of the value, which is the
// payload without the company id.
var (
	rssiRe = regexp.MustCompile(`Device ([0-9A-F:]{17}) RSSI: (-?\d+)`)
	nameRe = regexp.MustCompile(`Device ([0-9A-F:]{17}) Name: (.*)`)
	manuRe = regexp.MustCompile(`Device ([0-9A-F:]{17}) ManufacturerData\.Value: ([0-9a-fA-F ]+)`)
	uuidRe = regexp.MustCompile(`(?i)` + regexp.QuoteMeta(protocol.ServiceUUID))
)

type partial struct {
	rssi int
	name string
	raw  []byte
	uuid bool
}

func readScan(ctx context.Context, r io.Reader) (Hit, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	seen := map[string]*partial{}
	for sc.Scan() {
		if ctx.Err() != nil {
			return Hit{}, ctx.Err()
		}
		line := sc.Text()
		if m := rssiRe.FindStringSubmatch(line); m != nil {
			p := get(seen, m[1])
			n, _ := strconv.Atoi(m[2])
			p.rssi = n
		}
		if m := nameRe.FindStringSubmatch(line); m != nil {
			get(seen, m[1]).name = strings.TrimSpace(m[2])
		}
		if m := manuRe.FindStringSubmatch(line); m != nil {
			raw, err := hex.DecodeString(strings.ReplaceAll(m[2], " ", ""))
			if err == nil {
				get(seen, m[1]).raw = raw
			}
		}
		if uuidRe.MatchString(line) {
			if m := regexp.MustCompile(`Device ([0-9A-F:]{17})`).FindStringSubmatch(line); m != nil {
				get(seen, m[1]).uuid = true
			}
		}
		for addr, p := range seen {
			if !p.uuid || len(p.raw) == 0 {
				continue
			}
			payload, err := protocol.Decode(p.raw)
			if err != nil {
				continue
			}
			return Hit{
				Address: addr,
				RSSI:    p.rssi,
				Name:    p.name,
				Payload: payload,
				SeenAt:  time.Now(),
			}, nil
		}
	}
	if err := sc.Err(); err != nil {
		return Hit{}, fmt.Errorf("reading bluetoothctl: %w", err)
	}
	return Hit{}, fmt.Errorf("bluetoothctl exited before a companion appeared")
}

func get(m map[string]*partial, addr string) *partial {
	p, ok := m[addr]
	if !ok {
		p = &partial{}
		m[addr] = p
	}
	return p
}
