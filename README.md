# breadpair

`phone up` finds the CMF Phone 2 Pro over Bluetooth LE, brings up a Wi-Fi
Direct group, waits for the phone to join, connects `adb`, and launches
`scrcpy`. It does this without taking the laptop's normal Wi-Fi connection
away.

```
phone up        # scan, group, adb, scrcpy
phone down      # stop scrcpy, remove the group, give Wi-Fi back
phone status
```

The laptop is an Arch Linux Yoga Slim 7 (MT7925, `wlan0`). The phone runs
Nothing OS. Nothing here needs root on the phone. The laptop side uses
`sudo` for `wpa_supplicant`, `ip`, `nmcli`, and `ufw`.

## What actually happens

The MT7925 can keep a station and a P2P group owner at the same time
(`iw list`: managed <= 2, P2P-GO <= 1, channels <= 2). NetworkManager's own
`wpa_supplicant` will not: `p2p_group_add` on it returns OK and the group is
torn down a second later, because NetworkManager did not create it.

So `phone up` does this:

1. Scans BLE for the companion service `8a6e1b10-7c4d-4e2a-9f31-b6d0a4c85e17`.
2. Asks NetworkManager to let go of `wlan0` and starts its own
   `wpa_supplicant` on it.
3. Reconnects the saved WPA2 network on that supplicant and takes a DHCP
   lease. The default route is installed at metric 600, so an existing
   ethernet route stays preferred and Wi-Fi takes over when ethernet is gone.
4. Starts a persistent group with `p2p_group_add freq=2437` and serves DHCP
   on `192.168.49.0/24` with no gateway and no DNS, so the phone keeps its own
   internet path.
5. Writes the SSID, passphrase, and frequency to the companion over BLE. The
   companion joins with group-owner intent 0, so the laptop stays the owner.
6. Waits for the phone's DHCP lease, runs `adb connect`, then
   `scrcpy --stay-awake --turn-screen-off`.

`phone down` kills scrcpy, disconnects adb, removes the group, stops
dnsmasq, and sets `wlan0` managed again. NetworkManager reconnects whichever
saved network it wants. Verified on this machine: the station held
`192.168.1.106` and answered pings while the group was up, and `down` left
`wlan0` connected.

State, the supplicant config, and the group passphrase live in
`~/.local/state/breadpair`, mode 600.

## Laptop build

```
cd ~/Projects/breadpair
go build -o phone ./cmd/phone
sudo install -m 755 phone /usr/local/bin/phone
```

Packages: `wpa_supplicant`, `dnsmasq`, `bluez`, `bluez-utils`, `android-tools`,
`scrcpy`, `go`, `ufw`.

### sudo

`phone` runs as the user and calls `sudo -n`. Install the drop-in:

```
sudo install -m 440 dist/sudoers.d-breadpair /etc/sudoers.d/breadpair
sudo visudo -cf /etc/sudoers.d/breadpair
```

The commands it allows are `wpa_supplicant`, `wpa_cli`, `ip`, `iw`, `nmcli`,
`dnsmasq`, `ufw`, and the `phone dhcp4` helper. The DHCP client port is held
by NetworkManager for the ethernet lease, so the helper sends DHCP through a
raw packet socket, which is why it runs as root.

### Flags

```
phone up -ssid 'NetComm 4887'   # reconnect this saved network
phone up -skip-ble              # bring the group up without scanning
phone up -no-scrcpy             # stop once adb is connected
phone up -iface wlan0 -adapter hci0
phone up -ble-timeout 25s -join-timeout 90s -adb-timeout 40s
```

`-ssid` defaults to the first saved WPA2 network that associates. Only
`key-mgmt=wpa-psk` profiles are read. Enterprise networks are not handled.

## Phone setup, once

1. Install the companion. The SDK already at `~/Android` is enough; there is
   no Gradle project.

   ```
   cd android
   make
   adb install -r breadpair-companion.apk
   ```

   `make` signs with a keystore it creates at
   `~/.local/share/breadpair/debug.keystore`.

2. Developer options, then Wireless debugging. Pair the laptop once:
   `adb pair <ip>:<pairing-port> <code>`. That dialog cannot be automated,
   and breadpair does not try. After that, `adb connect` works until the
   pairing is revoked.

3. Open breadpair, tap Arm, accept the permissions, and tap Allow background
   use. That asks Nothing OS to stop freezing the app. The permissions are
   Bluetooth advertise and connect, nearby Wi-Fi devices, notifications, and
   fine location. Location is requested because Wi-Fi Direct still demands it
   on this API level even though the app never reads a position.

4. Leave Wireless debugging on. The companion shows whether it is advertising
   and whether it has joined the group.

Then `phone up` from the laptop.

## Screen off

The companion is a foreground service with a persistent notification and a
partial wake lock, so the CPU stays awake and BLE advertising continues while
the screen is off. That is as far as stock Android goes.

What it cannot promise:

- Nothing OS can still freeze the service after a while. The battery
  exemption is the mitigation, not a guarantee. If `phone up` stops finding
  the phone, open the app once so the process is restarted.
- Wi-Fi Direct on Android will not join a group while the phone is in deep
  doze unless the process is awake. The wake lock covers the armed state.
  Force-stopping the app, or the OEM killing it, ends the session.
- Wireless debugging itself is a Developer options feature. Android disables
  it after a reboot until the phone is unlocked once. There is no way around
  that without root.
- The first `adb` authorisation is a prompt on the phone. It is remembered
  after that.

## Layout

```
cmd/phone              the binary
internal/protocol      the BLE payload and credential bytes
internal/ble           bluetoothctl scan and credential write
internal/wifi          wpa_supplicant, station, group, dnsmasq
internal/dhcp4         the raw DHCPv4 client for the station
internal/session       up, down, status
internal/state         ~/.local/state/breadpair/session.json
android                the companion, built with aapt2 and d8
```

The credential written to the control characteristic is version, SSID length,
SSID, passphrase length, passphrase, frequency as a little-endian uint16. The
advertisement payload is `BP`, version, flags, adb port, and the phone's P2P
device address when it knows one.

## Limitations

- The group is pinned to channel 6 (2437 MHz). The station may associate on
  another channel; the MT7925 allows two channels at once. A card that does
  not will fail `p2p_group_add`, and the error names that step.
- `phone up` is idempotent only while its own session is alive. A leftover
  `wpa_supplicant` from the old manual flow (the one that left `wlan0`
  unmanaged) has to be stopped first, or `phone up` reports the interface as
  unmanaged and stops.
- BLE credential delivery depends on the companion process running. With
  `-skip-ble` the group still comes up and the phone can join from the Wi-Fi
  Direct UI, but nothing types the passphrase for it.
- ufw on this machine drops the group subnet. `phone up` adds an allow rule
  for the group interface and `down` removes it.
