package protocol

import "testing"

func TestPayloadRoundTrip(t *testing.T) {
	in := Payload{
		Version: Version,
		Flags:   FlagArmed | FlagADBReady,
		ADBPort: 37123,
	}
	copy(in.P2PDevice[:], []byte{0x44, 0xcb, 0x8b, 0x54, 0xb8, 0x14})

	out, err := Decode(in.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if out.Flags != in.Flags || out.ADBPort != 37123 {
		t.Fatalf("got %+v", out)
	}
	if out.P2PAddress() != "44:cb:8b:54:b8:14" {
		t.Fatalf("address %s", out.P2PAddress())
	}
}

func TestPayloadRejectsGarbage(t *testing.T) {
	if _, err := Decode([]byte("nope")); err == nil {
		t.Fatal("short payload accepted")
	}
	bad := Payload{Version: Version}.Encode()
	bad[0] = 'X'
	if _, err := Decode(bad); err == nil {
		t.Fatal("bad magic accepted")
	}
	bad = Payload{Version: Version}.Encode()
	bad[2] = 9
	if _, err := Decode(bad); err == nil {
		t.Fatal("wrong version accepted")
	}
}

func TestCredentialRoundTrip(t *testing.T) {
	in := Credential{SSID: "DIRECT-breadpair", Passphrase: "correct horse", Frequency: 2437}
	raw, err := in.Encode()
	if err != nil {
		t.Fatal(err)
	}
	out, err := DecodeCredential(raw)
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("got %+v", out)
	}
}

func TestCredentialRejectsWeakPassphrase(t *testing.T) {
	_, err := (Credential{SSID: "DIRECT-x", Passphrase: "short", Frequency: 2437}).Encode()
	if err == nil {
		t.Fatal("short passphrase accepted")
	}
}
