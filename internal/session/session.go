// Package session is the orchestration behind `phone up`, `down`, and `status`.
package session

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"breadway.dev/breadpair/internal/ble"
	"breadway.dev/breadpair/internal/executil"
	"breadway.dev/breadpair/internal/nm"
	"breadway.dev/breadpair/internal/protocol"
	"breadway.dev/breadpair/internal/state"
	"breadway.dev/breadpair/internal/wakeup"
	"breadway.dev/breadpair/internal/wifi"
)

// Options are the flags of `phone up`.
type Options struct {
	Iface       string
	StationSSID string
	SkipBLE     bool
	BLETimeout  time.Duration
	JoinTimeout time.Duration
	ADBTimeout  time.Duration
	Adapter     string
	NoScrcpy    bool
	// Wake is host:port of the companion's Tailscale listener. When set, the
	// credentials go over that link and BLE is not required.
	Wake string
}

// DefaultOptions matches the Yoga Slim 7 this was built on.
func DefaultOptions() Options {
	return Options{
		Iface:       "wlan0",
		BLETimeout:  25 * time.Second,
		JoinTimeout: 90 * time.Second,
		ADBTimeout:  40 * time.Second,
		Adapter:     "hci0",
	}
}

// Up runs the whole path. It is idempotent: a live session is reported and
// left alone.
func Up(ctx context.Context, opt Options) error {
	dir, err := state.Dir()
	if err != nil {
		return err
	}
	if existing, err := state.Load(dir); err == nil && alive(existing) {
		fmt.Printf("already up: group %s on %s", existing.SSID, existing.GroupIface)
		if existing.PhoneIP != "" {
			fmt.Printf(", phone %s", existing.ADBSerial)
		}
		fmt.Println()
		return nil
	}

	for _, tool := range []string{"nmcli", "iw", "ip", "adb", "scrcpy"} {
		if opt.NoScrcpy && tool == "scrcpy" {
			continue
		}
		if err := executil.Look(tool); err != nil {
			return err
		}
	}

	var hit *ble.Hit
	if opt.Wake == "" && !opt.SkipBLE {
		fmt.Printf("scanning BLE for the companion (%s)\n", opt.BLETimeout)
		found, err := ble.Scan(ctx, opt.Adapter, opt.BLETimeout)
		if err != nil {
			return err
		}
		hit = &found
		fmt.Printf("found companion %s\n", found)
	}

	route, _ := nm.DefaultRouteDev(ctx)
	fmt.Printf("default route is on %s\n", or(route, "nothing"))

	ws, err := wifi.Up(ctx, wifi.Config{
		StateDir:        dir,
		StationIface:    opt.Iface,
		StationSSID:     opt.StationSSID,
		HadDefaultRoute: route != "" && route != opt.Iface,
	})
	if err != nil {
		return fmt.Errorf("wifi: %w", err)
	}
	st := toState(ws)
	if err := state.Save(dir, st); err != nil {
		_ = wifi.Down(context.Background(), ws)
		return err
	}
	fmt.Printf("group %s on %s, passphrase stored in %s\n", ws.Group.SSID, ws.Group.Iface, dir)
	if ws.Station != nil {
		fmt.Printf("station reconnected to %s at %s\n", ws.Station.SSID, ws.Station.IP)
	}

	cred := protocol.Credential{SSID: ws.Group.SSID, Passphrase: ws.Group.Passphrase, Frequency: ws.Group.Frequency}
	if opt.Wake != "" {
		fmt.Printf("sending the group credentials to %s\n", opt.Wake)
		wakeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := wakeup.Send(wakeCtx, opt.Wake, cred)
		cancel()
		if err != nil {
			return fmt.Errorf("%w\nphone: the group is up; run 'phone down' to tear it down", err)
		}
	}
	if hit != nil {
		fmt.Printf("sending the group credentials to %s\n", hit.Address)
		pushCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := ble.Push(pushCtx, hit.Address, cred)
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "phone: could not push credentials over BLE: %v\n", err)
			fmt.Fprintf(os.Stderr, "phone: join %s manually from the companion if it did not connect\n", ws.Group.SSID)
		}
	}

	fmt.Printf("waiting up to %s for the phone to join\n", opt.JoinTimeout)
	ip, mac, err := wifi.WaitForLease(ctx, dir, opt.JoinTimeout)
	if err != nil {
		return fmt.Errorf("%w\nphone: the group is still up; the companion joins %q once it has the passphrase\nphone: run 'phone down' to tear it down", err, ws.Group.SSID)
	}
	if !wifi.GroupNet().Contains(ip) {
		return fmt.Errorf("lease %s is outside the group subnet", ip)
	}
	fmt.Printf("phone %s took %s\n", mac, ip)
	st.PhoneIP = ip.String()

	serial := net.JoinHostPort(ip.String(), adbPort(hit))
	fmt.Printf("connecting adb at %s\n", serial)
	if err := adbConnect(ctx, serial, opt.ADBTimeout); err != nil {
		_ = state.Save(dir, st)
		return fmt.Errorf("%w\nphone: the group is still up. Wireless debugging must already be paired once; see the README", err)
	}
	st.ADBSerial = serial
	if err := state.Save(dir, st); err != nil {
		return err
	}

	if opt.NoScrcpy {
		fmt.Println("adb is up; scrcpy skipped")
		return nil
	}
	pid, err := startScrcpy(serial)
	if err != nil {
		return fmt.Errorf("scrcpy: %w", err)
	}
	st.ScrcpyPID = pid
	if err := state.Save(dir, st); err != nil {
		return err
	}
	fmt.Printf("scrcpy running (pid %d)\n", pid)
	return nil
}

// Down tears the session down. With no session file it still tries to return
// wlan0 to NetworkManager, which is the state a crashed run can leave behind.
func Down(ctx context.Context, iface string) error {
	dir, err := state.Dir()
	if err != nil {
		return err
	}
	st, err := state.Load(dir)
	if err != nil {
		fmt.Println("no session file; checking the interface state")
		_, _ = executil.Sudo(ctx, "nmcli", "device", "set", iface, "managed", "yes")
		return nil
	}
	if st.ScrcpyPID > 0 {
		_ = syscall.Kill(st.ScrcpyPID, syscall.SIGTERM)
	}
	if st.ADBSerial != "" {
		_, _ = executil.Run(ctx, "adb", "disconnect", st.ADBSerial)
	}
	ws := fromState(st)
	if err := wifi.Down(ctx, ws); err != nil {
		fmt.Fprintf(os.Stderr, "phone: teardown was partial: %v\n", err)
	}
	if err := state.Clear(dir); err != nil {
		return err
	}
	fmt.Println("down")
	return nil
}

// Status prints the session and the live checks against it.
func Status(ctx context.Context) error {
	dir, err := state.Dir()
	if err != nil {
		return err
	}
	st, err := state.Load(dir)
	if err != nil {
		fmt.Println("down (no session)")
		return nil
	}
	fmt.Printf("session: %s\n", filepathOr(dir))
	fmt.Printf("  supplicant: pid %d %s\n", st.SupplicantPID, running(st.SupplicantPID))
	if st.GroupIface != "" {
		fmt.Printf("  group: %s iface %s freq %d %s\n", st.SSID, st.GroupIface, st.Frequency, ifaceUp(st.GroupIface))
	}
	if st.StationSSID != "" {
		fmt.Printf("  station: %s at %s via %s\n", st.StationSSID, st.StationIP, or(st.StationRouter, "no gateway"))
	}
	if route, err := nm.DefaultRouteDev(ctx); err == nil {
		fmt.Printf("  default route: %s\n", or(route, "none"))
	}
	if st.PhoneIP != "" {
		fmt.Printf("  phone: %s adb %s\n", st.PhoneIP, or(st.ADBSerial, "not connected"))
	}
	if st.ScrcpyPID > 0 {
		fmt.Printf("  scrcpy: pid %d %s\n", st.ScrcpyPID, running(st.ScrcpyPID))
	}
	return nil
}

func toState(ws *wifi.Session) state.Session {
	st := state.Session{
		SupplicantPID: ws.SupplicantPID,
		ConfPath:      ws.ConfPath,
		LogPath:       ws.LogPath,
		StationIface:  ws.StationIface,
		OwnedWLAN:     ws.OwnedWLAN,
		DHCPPid:       ws.DHCPPid,
		UFWAdded:      ws.UFWAdded,
	}
	if ws.Group != nil {
		st.GroupIface = ws.Group.Iface
		st.SSID = ws.Group.SSID
		st.Passphrase = ws.Group.Passphrase
		st.Frequency = ws.Group.Frequency
		st.GroupMAC = ws.Group.MAC
	}
	if ws.Station != nil {
		st.StationSSID = ws.Station.SSID
		st.StationIP = ws.Station.IP
		st.StationRouter = ws.Station.Router
		st.StationMetric = ws.Station.Metric
		st.StationRoute = ws.Station.Created
	}
	return st
}

func fromState(st state.Session) *wifi.Session {
	ws := &wifi.Session{
		SupplicantPID: st.SupplicantPID,
		ConfPath:      st.ConfPath,
		LogPath:       st.LogPath,
		StationIface:  st.StationIface,
		OwnedWLAN:     st.OwnedWLAN,
		DHCPPid:       st.DHCPPid,
		UFWAdded:      st.UFWAdded,
	}
	if st.GroupIface != "" {
		ws.Group = &wifi.Group{Iface: st.GroupIface, SSID: st.SSID, Passphrase: st.Passphrase, Frequency: st.Frequency, MAC: st.GroupMAC}
	}
	if st.StationRoute {
		ws.Station = &wifi.Station{SSID: st.StationSSID, IP: st.StationIP, Router: st.StationRouter, Metric: st.StationMetric, Created: true}
	}
	return ws
}

func adbConnect(ctx context.Context, serial string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		res, err := executil.Run(ctx, "adb", "connect", serial)
		text := res.Stdout + res.Stderr
		if err == nil && (strings.Contains(text, "connected to") || strings.Contains(text, "already connected")) {
			out, derr := executil.Output(ctx, "adb", "devices")
			if derr == nil && strings.Contains(out, serial+"\tdevice") {
				return nil
			}
			last = fmt.Errorf("adb connected but the device is not authorized yet")
		} else if err != nil {
			last = err
		} else {
			last = fmt.Errorf("%s", strings.TrimSpace(text))
		}
		time.Sleep(2 * time.Second)
	}
	if last == nil {
		last = fmt.Errorf("timed out")
	}
	return fmt.Errorf("adb connect %s: %w", serial, last)
}

func startScrcpy(serial string) (int, error) {
	cmd := exec.Command("scrcpy", "--stay-awake", "--turn-screen-off", "--serial", serial)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	// Detach so `phone up` can return while the mirror stays.
	go cmd.Wait()
	return cmd.Process.Pid, nil
}

func adbPort(hit *ble.Hit) string {
	if hit != nil && hit.Payload.ADBPort != 0 {
		return strconv.Itoa(int(hit.Payload.ADBPort))
	}
	return "5555"
}

func alive(st state.Session) bool {
	return st.SupplicantPID > 0 && running(st.SupplicantPID) == "running" && ifaceUp(st.GroupIface) == "up"
}

func running(pid int) string {
	if pid <= 0 {
		return "not running"
	}
	// The supplicant runs as root, so signal 0 comes back EPERM and proves
	// nothing. Ask proc whether the pid exists.
	if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); err != nil {
		return "not running"
	}
	return "running"
}

func ifaceUp(name string) string {
	if name == "" {
		return "down"
	}
	ifi, err := net.InterfaceByName(name)
	if err != nil || ifi.Flags&net.FlagUp == 0 {
		return "down"
	}
	return "up"
}

func or(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func filepathOr(dir string) string { return dir }
