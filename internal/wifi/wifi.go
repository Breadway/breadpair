// Package wifi owns the wpa_supplicant that runs both the station and the
// Wi-Fi Direct group.
//
// NetworkManager's own supplicant refuses to keep a group it did not create:
// p2p_group_add there returns OK, the AP comes up, and the same process
// disables it a moment later. So breadpair takes wlan0 for the session. It
// reconnects every saved WPA2 network on its own supplicant, takes a DHCP
// lease, and installs the default route at a worse metric than whatever the
// machine was already using. On `down` it gives wlan0 straight back.
package wifi

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"breadway.dev/breadpair/internal/dhcp4"
	"breadway.dev/breadpair/internal/executil"
)

const (
	ctrlDir    = "/run/wpa_supplicant"
	groupNet   = "192.168.49.0/24"
	groupIP    = "192.168.49.1/24"
	dhcpFirst  = "192.168.49.10"
	dhcpLast   = "192.168.49.50"
	listenChan = 6
	frequency  = 2437
)

// Group is the running P2P group owner.
type Group struct {
	Iface      string
	SSID       string
	Passphrase string
	Frequency  int
	MAC        string
}

// Station is the saved Wi-Fi network breadpair reconnected, if any.
type Station struct {
	SSID    string
	IP      string
	Router  string
	Metric  int
	Created bool // true when this session installed the route
}

// Session is everything `down` has to undo.
type Session struct {
	SupplicantPID int
	ConfPath      string
	LogPath       string
	StationIface  string
	Group         *Group
	Station       *Station
	DHCPPid       int
	UFWAdded      bool
	OwnedWLAN     bool // true when we set wlan0 unmanaged
}

// SavedNetwork is one NetworkManager WPA-PSK profile.
type SavedNetwork struct {
	SSID string
	PSK  string
}

// Config is the per-run input.
type Config struct {
	StateDir     string
	StationIface string
	// StationSSID restricts reconnection to one saved network. Empty means
	// the first saved network that associates.
	StationSSID string
	// KeepStation reports whether the station was already connected when the
	// session started. The route metric is chosen from it.
	HadDefaultRoute bool
}

// Up starts the supplicant, the station, and the group. On any failure it
// tears down what it started and returns the error.
func Up(ctx context.Context, cfg Config) (*Session, error) {
	if err := executil.Look("wpa_supplicant"); err != nil {
		return nil, err
	}
	if err := executil.Look("wpa_cli"); err != nil {
		return nil, err
	}
	if err := executil.Look("dnsmasq"); err != nil {
		return nil, err
	}

	s := &Session{StationIface: cfg.StationIface, ConfPath: filepath.Join(cfg.StateDir, "wpa.conf"), LogPath: filepath.Join(cfg.StateDir, "wpa.log")}
	var err error
	defer func() {
		if err != nil {
			_ = Down(context.Background(), s)
		}
	}()

	if err = takeInterface(ctx, cfg.StationIface); err != nil {
		return nil, err
	}
	s.OwnedWLAN = true
	// NetworkManager keeps the control socket and the DHCP client port for a
	// moment after it reports the interface unmanaged. Starting earlier makes
	// wpa_supplicant refuse the socket, and the DHCP client fail with
	// "address already in use".
	waitSocketGone(ctx, cfg.StationIface, 8*time.Second)
	waitPortFree(ctx, 68, 8*time.Second)

	if err = writeConf(s.ConfPath); err != nil {
		return nil, err
	}
	if err = startSupplicant(ctx, s); err != nil {
		return nil, err
	}

	// The station is best effort. Ethernet may already be the default route,
	// and a missing saved network must not block the phone session.
	if st, stErr := connectStation(ctx, s, cfg); stErr != nil {
		fmt.Fprintf(os.Stderr, "phone: station not reconnected: %v\n", stErr)
		fmt.Fprintf(os.Stderr, "phone: the Wi-Fi Direct group will still come up; internet stays on the existing default route\n")
	} else {
		s.Station = st
	}

	g, err := startGroup(ctx, s)
	if err != nil {
		return nil, err
	}
	s.Group = g
	if err = configureGroup(ctx, s); err != nil {
		return nil, err
	}
	return s, nil
}

// Down reverses Up. Every step is best effort because it also runs after a
// partial failure and after a crash, from the state file.
func Down(ctx context.Context, s *Session) error {
	if s == nil {
		return nil
	}
	var problems []string
	note := func(err error) {
		if err != nil {
			problems = append(problems, err.Error())
		}
	}

	if s.Group != nil && s.Group.Iface != "" {
		_, err := executil.Sudo(ctx, "wpa_cli", "-p", ctrlDir, "-i", "p2p-dev-"+s.StationIface, "p2p_group_remove", s.Group.Iface)
		note(err)
	}
	if s.DHCPPid > 0 {
		_, _ = executil.Sudo(ctx, "kill", fmt.Sprint(s.DHCPPid))
	}
	if s.UFWAdded && s.Group != nil {
		_, _ = executil.Sudo(ctx, "ufw", "delete", "allow", "in", "on", s.Group.Iface)
	}
	if s.Station != nil && s.Station.Created && s.Station.Router != "" {
		_, _ = executil.Sudo(ctx, "ip", "route", "del", "default", "via", s.Station.Router, "dev", s.StationIface)
	}
	if s.SupplicantPID > 0 {
		_, _ = executil.Sudo(ctx, "kill", fmt.Sprint(s.SupplicantPID))
		time.Sleep(300 * time.Millisecond)
	}
	_, _ = executil.Sudo(ctx, "ip", "addr", "flush", "dev", s.StationIface)
	if s.OwnedWLAN {
		if _, err := executil.Sudo(ctx, "nmcli", "device", "set", s.StationIface, "managed", "yes"); err != nil {
			note(fmt.Errorf("hand %s back to NetworkManager: %w", s.StationIface, err))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return nil
}

func waitSocketGone(ctx context.Context, iface string, d time.Duration) {
	path := filepath.Join(ctrlDir, iface)
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return
		}
		// A socket whose supplicant is gone answers nothing. Remove it so the
		// new one can bind.
		if _, err := executil.Sudo(ctx, "wpa_cli", "-p", ctrlDir, "-i", iface, "ping"); err != nil {
			_, _ = executil.Sudo(ctx, "rm", "-f", path)
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	_, _ = executil.Sudo(ctx, "rm", "-f", path)
}

func waitPortFree(ctx context.Context, port int, d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		res, err := executil.Sudo(ctx, "ss", "-uln", fmt.Sprintf("( sport = %d )", port))
		// ss prints a header even when nothing matches, and exits 0.
		busy := err == nil && strings.Contains(res.Stdout, fmt.Sprintf(":%d", port))
		if !busy {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func takeInterface(ctx context.Context, iface string) error {
	if _, err := executil.Sudo(ctx, "nmcli", "device", "set", iface, "managed", "no"); err != nil {
		return fmt.Errorf("unmanage %s: %w", iface, err)
	}
	// NetworkManager removes the control socket as it lets go. Wait until the
	// interface is actually unmanaged so the new supplicant does not race it.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out, err := executil.Output(ctx, "nmcli", "-t", "-f", "DEVICE,STATE", "device", "status")
		if err == nil && strings.Contains(out, iface+":unmanaged") {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("%s did not become unmanaged within 5s", iface)
}

func writeConf(path string) error {
	body := fmt.Sprintf(`ctrl_interface=DIR=%s GROUP=wheel
update_config=0
device_name=breadpair
device_type=1-0050F204-1
config_methods=virtual_push_button keypad
p2p_go_intent=15
country=AU
p2p_listen_reg_class=81
p2p_listen_channel=%d
p2p_oper_reg_class=81
p2p_oper_channel=%d
`, ctrlDir, listenChan, listenChan)
	return os.WriteFile(path, []byte(body), 0o600)
}

func startSupplicant(ctx context.Context, s *Session) error {
	_ = os.Remove(filepath.Join(ctrlDir, s.StationIface))
	if _, err := executil.Sudo(ctx, "mkdir", "-p", ctrlDir); err != nil {
		return err
	}
	cmd := exec.Command("sudo", "-n", "wpa_supplicant",
		"-B", "-Dnl80211", "-i", s.StationIface, "-c", s.ConfPath, "-f", s.LogPath, "-t")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("start wpa_supplicant: %w", err)
	}
	// -B daemonises, so the process we started exits. The pid comes from the
	// control socket once it is answering, which is the daemon itself rather
	// than a helper that happens to share its command line.
	if err := waitCtrl(ctx, s.StationIface); err != nil {
		return err
	}
	// There is no pid query on the control socket. Match the daemon by the
	// config path, which only our process has on its command line.
	out, err := executil.Output(ctx, "ps", "-eo", "pid,args")
	if err != nil {
		return fmt.Errorf("list processes: %w", err)
	}
	for _, line := range strings.Split(out, "\n") {
		pid, cmd, ok := strings.Cut(strings.TrimSpace(line), " ")
		fields := strings.Fields(cmd)
		// Match the daemon's own argv, not any process whose command line
		// mentions it. The shell that launched `phone` has the config path in
		// its arguments, and recording that pid kills the wrong thing.
		if ok && len(fields) > 0 && filepath.Base(fields[0]) == "wpa_supplicant" && strings.Contains(cmd, "-c "+s.ConfPath) {
			fmt.Sscanf(strings.TrimSpace(pid), "%d", &s.SupplicantPID)
			break
		}
	}
	if s.SupplicantPID == 0 {
		return fmt.Errorf("wpa_supplicant is answering but its pid was not found; see %s", s.LogPath)
	}
	return nil
}

func waitCtrl(ctx context.Context, iface string) error {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := executil.Sudo(ctx, "wpa_cli", "-p", ctrlDir, "-i", iface, "ping"); err == nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("wpa_supplicant control socket for %s never appeared", iface)
}

func connectStation(ctx context.Context, s *Session, cfg Config) (*Station, error) {
	saved, err := savedNetworks(ctx)
	if err != nil {
		return nil, err
	}
	if cfg.StationSSID != "" {
		var filtered []SavedNetwork
		for _, n := range saved {
			if n.SSID == cfg.StationSSID {
				filtered = append(filtered, n)
			}
		}
		saved = filtered
	}
	if len(saved) == 0 {
		return nil, fmt.Errorf("no saved WPA-PSK network")
	}

	// One network block, enabled only while it is being tried. Enabling all of
	// them lets wpa_supplicant roam onto a different saved SSID than the one
	// select_network names.
	var connected *SavedNetwork
	for i, n := range saved {
		id := fmt.Sprint(i)
		if _, err := cli(ctx, s.StationIface, "add_network"); err != nil {
			return nil, err
		}
		if _, err := cli(ctx, s.StationIface, "set_network", id, "ssid", quote(n.SSID)); err != nil {
			return nil, err
		}
		if _, err := cli(ctx, s.StationIface, "set_network", id, "psk", quote(n.PSK)); err != nil {
			return nil, err
		}
		if _, err := cli(ctx, s.StationIface, "set_network", id, "scan_ssid", "1"); err != nil {
			return nil, err
		}
		_, _ = cli(ctx, s.StationIface, "disable_network", id)
	}
	_, _ = cli(ctx, s.StationIface, "scan")
	time.Sleep(2 * time.Second)
	for i := range saved {
		id := fmt.Sprint(i)
		_, _ = cli(ctx, s.StationIface, "select_network", id)
		if waitState(ctx, s.StationIface, "COMPLETED", 8*time.Second) {
			got, _ := cli(ctx, s.StationIface, "status")
			if strings.Contains(got, "ssid="+saved[i].SSID+"\n") || strings.Contains(got, "ssid="+saved[i].SSID) {
				connected = &saved[i]
				break
			}
		}
		_, _ = cli(ctx, s.StationIface, "disable_network", id)
	}
	if connected == nil {
		return nil, fmt.Errorf("none of the %d saved networks associated", len(saved))
	}

	lease, err := acquireAsRoot(ctx, s.StationIface)
	if err != nil {
		return nil, err
	}
	prefix := lease.Prefix()
	if _, err := executil.Sudo(ctx, "ip", "addr", "replace", fmt.Sprintf("%s/%d", lease.IP, prefix), "dev", s.StationIface); err != nil {
		return nil, fmt.Errorf("assign station address: %w", err)
	}
	st := &Station{SSID: connected.SSID, IP: lease.IP.String(), Router: lease.Router.String()}
	if lease.Router != nil {
		// Sit behind the existing default route. When ethernet is up the
		// station is a hot spare; when it is the only link, metric 600 still
		// wins over nothing.
		metric := "600"
		if !cfg.HadDefaultRoute {
			metric = "100"
		}
		if _, err := executil.Sudo(ctx, "ip", "route", "replace", "default", "via", lease.Router.String(), "dev", s.StationIface, "metric", metric); err == nil {
			st.Created = true
			fmt.Sscanf(metric, "%d", &st.Metric)
		}
	}
	return st, nil
}

func startGroup(ctx context.Context, s *Session) (*Group, error) {
	dev := "p2p-dev-" + s.StationIface
	if err := waitCtrl(ctx, dev); err != nil {
		return nil, fmt.Errorf("p2p device interface: %w", err)
	}
	out, err := cli(ctx, dev, "p2p_group_add", "freq="+fmt.Sprint(frequency))
	if err != nil {
		return nil, fmt.Errorf("p2p_group_add: %w\n%s", err, out)
	}
	iface := ""
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && iface == "" {
		entries, _ := os.ReadDir(ctrlDir)
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, "p2p-"+s.StationIface+"-") {
				iface = name
			}
		}
		if iface == "" {
			time.Sleep(200 * time.Millisecond)
		}
	}
	if iface == "" {
		return nil, fmt.Errorf("p2p_group_add returned %q but no group interface appeared", strings.TrimSpace(out))
	}
	if !waitState(ctx, iface, "COMPLETED", 6*time.Second) {
		return nil, fmt.Errorf("group interface %s did not reach COMPLETED", iface)
	}
	status, err := cli(ctx, iface, "status")
	if err != nil {
		return nil, err
	}
	g := &Group{Iface: iface, Frequency: frequency}
	for _, line := range strings.Split(status, "\n") {
		k, v, _ := strings.Cut(line, "=")
		switch k {
		case "ssid":
			g.SSID = v
		case "freq":
			fmt.Sscanf(v, "%d", &g.Frequency)
		case "address":
			g.MAC = v
		}
	}
	pass, err := cli(ctx, iface, "p2p_get_passphrase")
	if err != nil {
		return nil, fmt.Errorf("read group passphrase: %w", err)
	}
	g.Passphrase = strings.TrimSpace(pass)
	if g.SSID == "" || g.Passphrase == "" {
		return nil, fmt.Errorf("group came up without an ssid or passphrase")
	}
	return g, nil
}

func configureGroup(ctx context.Context, s *Session) error {
	g := s.Group
	if _, err := executil.Sudo(ctx, "ip", "addr", "replace", groupIP, "dev", g.Iface); err != nil {
		return fmt.Errorf("assign %s to %s: %w", groupIP, g.Iface, err)
	}
	if _, err := executil.Sudo(ctx, "ip", "link", "set", g.Iface, "up"); err != nil {
		return err
	}
	conf := filepath.Join(filepath.Dir(s.ConfPath), "dnsmasq.conf")
	body := fmt.Sprintf(`interface=%s
bind-interfaces
except-interface=lo
listen-address=%s
dhcp-range=%s,%s,255.255.255.0,12h
dhcp-option=3
dhcp-option=6
port=0
no-resolv
log-dhcp
`, g.Iface, "192.168.49.1", dhcpFirst, dhcpLast)
	if err := os.WriteFile(conf, []byte(body), 0o600); err != nil {
		return err
	}
	runDir := filepath.Dir(s.ConfPath)
	pidFile := filepath.Join(runDir, "dnsmasq.pid")
	leaseFile := filepath.Join(runDir, "dnsmasq.leases")
	logFile := filepath.Join(runDir, "dnsmasq.log")
	// dnsmasq drops to nobody and has to create both files itself.
	if _, err := executil.Sudo(ctx, "touch", leaseFile, logFile); err != nil {
		return err
	}
	if _, err := executil.Sudo(ctx, "chown", "nobody:nobody", leaseFile, logFile); err != nil {
		return fmt.Errorf("chown lease file: %w", err)
	}
	if _, err := executil.Sudo(ctx, "dnsmasq", "-C", conf,
		"--dhcp-leasefile="+leaseFile, "--log-facility="+logFile, "--pid-file="+pidFile); err != nil {
		return fmt.Errorf("start dnsmasq: %w", err)
	}
	raw, err := executil.Sudo(ctx, "cat", pidFile)
	if err == nil {
		fmt.Sscanf(strings.TrimSpace(raw.Stdout), "%d", &s.DHCPPid)
	}
	// ufw drops the subnet otherwise. The rule is removed on the way down.
	if _, err := executil.Run(ctx, "ufw", "status"); err == nil {
		if _, err := executil.Sudo(ctx, "ufw", "allow", "in", "on", g.Iface, "comment", "breadpair"); err == nil {
			s.UFWAdded = true
		}
	}
	return nil
}

// WaitForLease polls the dnsmasq lease file until a client appears. The file
// is owned by nobody, so it is read through sudo.
func WaitForLease(ctx context.Context, stateDir string, timeout time.Duration) (net.IP, net.HardwareAddr, error) {
	path := filepath.Join(stateDir, "dnsmasq.leases")
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		ip, mac, err := newestLease(ctx, path)
		if err == nil {
			return ip, mac, nil
		}
		time.Sleep(time.Second)
	}
	return nil, nil, fmt.Errorf("no phone took a DHCP lease on the group within %s", timeout)
}

func newestLease(ctx context.Context, path string) (net.IP, net.HardwareAddr, error) {
	res, err := executil.Sudo(ctx, "cat", path)
	if err != nil {
		return nil, nil, err
	}
	var ip net.IP
	var mac net.HardwareAddr
	sc := bufio.NewScanner(strings.NewReader(res.Stdout))
	for sc.Scan() {
		// <expiry> <mac> <ip> <hostname> <id>
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 {
			continue
		}
		parsed := net.ParseIP(fields[2])
		hw, err := net.ParseMAC(fields[1])
		if parsed == nil || err != nil {
			continue
		}
		ip, mac = parsed, hw
	}
	if ip == nil {
		return nil, nil, fmt.Errorf("lease file is empty")
	}
	return ip, mac, nil
}

// GroupNet is the subnet the group hands out. adb refuses an address outside it.
func GroupNet() *net.IPNet {
	_, n, _ := net.ParseCIDR(groupNet)
	return n
}

func savedNetworks(ctx context.Context) ([]SavedNetwork, error) {
	// The directory is root-only, so the listing goes through sudo too.
	out, err := executil.Sudo(ctx, "find", "/etc/NetworkManager/system-connections",
		"-maxdepth", "1", "-name", "*.nmconnection", "-printf", "%p\n")
	if err != nil {
		return nil, fmt.Errorf("list saved networks: %w", err)
	}
	var files []string
	for _, line := range strings.Split(out.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			files = append(files, line)
		}
	}
	return parseProfiles(ctx, files)
}

func parseProfiles(ctx context.Context, files []string) ([]SavedNetwork, error) {
	var nets []SavedNetwork
	seen := map[string]bool{}
	for _, f := range files {
		if !strings.HasSuffix(f, ".nmconnection") {
			continue
		}
		raw, err := executil.Sudo(ctx, "cat", f)
		if err != nil {
			continue
		}
		var ssid, psk, mgmt string
		for _, line := range strings.Split(raw.Stdout, "\n") {
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			switch k {
			case "ssid":
				ssid = v
			case "psk":
				psk = v
			case "key-mgmt":
				mgmt = v
			}
		}
		if ssid == "" || psk == "" || mgmt != "wpa-psk" || seen[ssid] {
			continue
		}
		seen[ssid] = true
		nets = append(nets, SavedNetwork{SSID: ssid, PSK: psk})
	}
	return nets, nil
}

func cli(ctx context.Context, iface string, args ...string) (string, error) {
	res, err := executil.Sudo(ctx, append([]string{"wpa_cli", "-p", ctrlDir, "-i", iface}, args...)...)
	if err != nil {
		return res.Stdout, err
	}
	if strings.TrimSpace(res.Stdout) == "FAIL" {
		return res.Stdout, fmt.Errorf("wpa_cli %s %s returned FAIL", iface, strings.Join(args, " "))
	}
	return res.Stdout, nil
}

func waitState(ctx context.Context, iface, want string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		out, err := cli(ctx, iface, "status")
		if err == nil && strings.Contains(out, "wpa_state="+want) {
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}

func quote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

// acquireAsRoot re-runs the dhcp client as root. Port 68 is privileged, and
// the phone process itself is not.
func acquireAsRoot(ctx context.Context, iface string) (dhcp4.Lease, error) {
	self, err := os.Executable()
	if err != nil {
		return dhcp4.Lease{}, err
	}
	out, err := executil.Sudo(ctx, self, "dhcp4", iface)
	if err != nil {
		return dhcp4.Lease{}, fmt.Errorf("dhcp on %s: %w", iface, err)
	}
	var lease dhcp4.Lease
	for _, line := range strings.Split(out.Stdout, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "ip":
			lease.IP = net.ParseIP(v)
		case "mask":
			lease.Mask = net.IPMask(net.ParseIP(v).To4())
		case "router":
			lease.Router = net.ParseIP(v)
		case "dns":
			if ip := net.ParseIP(v); ip != nil {
				lease.DNS = append(lease.DNS, ip)
			}
		case "server":
			lease.Server = net.ParseIP(v)
		case "seconds":
			fmt.Sscanf(v, "%d", &lease.Seconds)
		}
	}
	if lease.IP == nil {
		return dhcp4.Lease{}, fmt.Errorf("dhcp on %s returned no address: %s", iface, strings.TrimSpace(out.Stdout))
	}
	return lease, nil
}

// PrintLease runs the client and prints the lease in the form acquireAsRoot
// parses. It is the `phone dhcp4` hidden subcommand.
func PrintLease(ctx context.Context, iface string) error {
	lease, err := dhcp4.Acquire(ctx, iface)
	if err != nil {
		return err
	}
	fmt.Printf("ip=%s\n", lease.IP)
	if lease.Mask != nil {
		fmt.Printf("mask=%s\n", net.IP(lease.Mask).To4())
	}
	if lease.Router != nil {
		fmt.Printf("router=%s\n", lease.Router)
	}
	for _, dns := range lease.DNS {
		fmt.Printf("dns=%s\n", dns)
	}
	if lease.Server != nil {
		fmt.Printf("server=%s\n", lease.Server)
	}
	fmt.Printf("seconds=%d\n", lease.Seconds)
	return nil
}

// RandomPassphrase is kept for tests and for the case where a group has to be
// described before wpa_supplicant has assigned one. The live passphrase comes
// from p2p_get_passphrase.
func RandomPassphrase() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf)[:13], nil
}
