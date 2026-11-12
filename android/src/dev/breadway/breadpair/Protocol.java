package dev.breadway.breadpair;

import java.nio.ByteBuffer;
import java.nio.ByteOrder;
import java.util.UUID;

/** The byte layout shared with the laptop. Keep it in step with internal/protocol. */
final class Protocol {
    static final UUID SERVICE = UUID.fromString("8a6e1b10-7c4d-4e2a-9f31-b6d0a4c85e17");
    static final UUID CONTROL = UUID.fromString("8a6e1b11-7c4d-4e2a-9f31-b6d0a4c85e17");
    static final UUID STATUS = UUID.fromString("8a6e1b12-7c4d-4e2a-9f31-b6d0a4c85e17");

    static final int COMPANY_ID = 0xFFFF;
    static final byte VERSION = 1;

    static final int FLAG_ARMED = 1;
    static final int FLAG_SCREEN_OFF = 2;
    static final int FLAG_ADB_READY = 4;

    private Protocol() {}

    static byte[] payload(int flags, int adbPort, byte[] p2p) {
        byte[] out = new byte[12];
        out[0] = 'B';
        out[1] = 'P';
        out[2] = VERSION;
        out[3] = (byte) flags;
        out[4] = (byte) (adbPort & 0xff);
        out[5] = (byte) ((adbPort >> 8) & 0xff);
        if (p2p != null && p2p.length == 6) {
            System.arraycopy(p2p, 0, out, 6, 6);
        }
        return out;
    }

    static final class Credential {
        final String ssid;
        final String passphrase;
        final int frequency;

        Credential(String ssid, String passphrase, int frequency) {
            this.ssid = ssid;
            this.passphrase = passphrase;
            this.frequency = frequency;
        }
    }

    static Credential decode(byte[] b) {
        if (b == null || b.length < 4) {
            throw new IllegalArgumentException("credential is too short");
        }
        if (b[0] != VERSION) {
            throw new IllegalArgumentException("credential version " + b[0]);
        }
        int nSSID = b[1] & 0xff;
        if (b.length < 2 + nSSID + 1) {
            throw new IllegalArgumentException("credential truncated in the ssid");
        }
        String ssid = new String(b, 2, nSSID);
        int nPass = b[2 + nSSID] & 0xff;
        if (b.length < 3 + nSSID + nPass + 2) {
            throw new IllegalArgumentException("credential truncated in the passphrase");
        }
        String pass = new String(b, 3 + nSSID, nPass);
        int freq = ByteBuffer.wrap(b, 3 + nSSID + nPass, 2).order(ByteOrder.LITTLE_ENDIAN).getShort() & 0xffff;
        if (ssid.isEmpty() || pass.length() < 8) {
            throw new IllegalArgumentException("credential is missing the ssid or passphrase");
        }
        return new Credential(ssid, pass, freq);
    }
}
