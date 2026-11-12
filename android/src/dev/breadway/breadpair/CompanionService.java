package dev.breadway.breadpair;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.app.Service;
import android.bluetooth.BluetoothAdapter;
import android.bluetooth.BluetoothGatt;
import android.bluetooth.BluetoothGattCharacteristic;
import android.bluetooth.BluetoothGattServer;
import android.bluetooth.BluetoothGattServerCallback;
import android.bluetooth.BluetoothGattService;
import android.bluetooth.BluetoothManager;
import android.bluetooth.le.AdvertiseCallback;
import android.bluetooth.le.AdvertiseData;
import android.bluetooth.le.AdvertiseSettings;
import android.bluetooth.le.BluetoothLeAdvertiser;
import android.content.Context;
import android.content.Intent;
import android.content.pm.ServiceInfo;
import android.net.wifi.p2p.WifiP2pConfig;
import android.net.wifi.p2p.WifiP2pGroup;
import android.net.wifi.p2p.WifiP2pManager;
import android.os.Build;
import android.os.IBinder;
import android.os.ParcelUuid;
import android.os.PowerManager;
import android.util.Log;

import java.nio.charset.StandardCharsets;

/**
 * Foreground service that advertises the breadpair BLE service and joins the
 * laptop's Wi-Fi Direct group when the laptop writes the credentials.
 *
 * A partial wake lock is held the whole time the service is armed. That is
 * the most Android allows without root: the CPU stays awake, but an OEM can
 * still freeze the process, which is why the activity asks for the
 * battery-unrestricted exemption.
 */
public class CompanionService extends Service {
    static final String TAG = "breadpair";
    static final String ACTION_STOP = "dev.breadway.breadpair.STOP";
    static final int NOTE_ID = 1;
    static final String CHANNEL = "breadpair";

    private PowerManager.WakeLock wakeLock;
    private BluetoothLeAdvertiser advertiser;
    private BluetoothGattServer gatt;
    private BluetoothGattCharacteristic statusChar;
    private WifiP2pManager p2p;
    private WifiP2pManager.Channel channel;
    private volatile String state = "Armed, waiting for the laptop";

    private final AdvertiseCallback advertiseCallback = new AdvertiseCallback() {
        @Override public void onStartSuccess(AdvertiseSettings settings) {
            setState("Advertising");
        }
        @Override public void onStartFailure(int error) {
            setState("BLE advertising failed (" + error + ")");
        }
    };

    private final BluetoothGattServerCallback gattCallback = new BluetoothGattServerCallback() {
        @Override
        public void onCharacteristicWriteRequest(android.bluetooth.BluetoothDevice device, int requestId,
                BluetoothGattCharacteristic characteristic, boolean preparedWrite, boolean responseNeeded,
                int offset, byte[] value) {
            if (!Protocol.CONTROL.equals(characteristic.getUuid())) {
                if (responseNeeded) gatt.sendResponse(device, requestId, BluetoothGatt.GATT_FAILURE, offset, null);
                return;
            }
            if (responseNeeded) gatt.sendResponse(device, requestId, BluetoothGatt.GATT_SUCCESS, offset, value);
            try {
                Protocol.Credential cred = Protocol.decode(value);
                setState("Joining " + cred.ssid);
                join(cred);
            } catch (RuntimeException e) {
                Log.w(TAG, "bad credential", e);
                setState("Laptop sent a credential this app could not read");
            }
        }
    };

    @Override
    public void onCreate() {
        super.onCreate();
        channel();
        Notification note = notification(state);
        if (Build.VERSION.SDK_INT >= 29) {
            startForeground(NOTE_ID, note, ServiceInfo.FOREGROUND_SERVICE_TYPE_CONNECTED_DEVICE);
        } else {
            startForeground(NOTE_ID, note);
        }
        PowerManager pm = (PowerManager) getSystemService(POWER_SERVICE);
        wakeLock = pm.newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "breadpair:armed");
        wakeLock.setReferenceCounted(false);
        wakeLock.acquire();
    }

    @Override
    public int onStartCommand(Intent intent, int flags, int startId) {
        if (intent != null && ACTION_STOP.equals(intent.getAction())) {
            stopSelf();
            return START_NOT_STICKY;
        }
        startRadio();
        return START_STICKY;
    }

    private void startRadio() {
        BluetoothManager bm = (BluetoothManager) getSystemService(BLUETOOTH_SERVICE);
        BluetoothAdapter adapter = bm.getAdapter();
        if (adapter == null || !adapter.isEnabled()) {
            setState("Bluetooth is off");
            return;
        }
        gatt = bm.openGattServer(this, gattCallback);
        BluetoothGattService service = new BluetoothGattService(Protocol.SERVICE, BluetoothGattService.SERVICE_TYPE_PRIMARY);
        BluetoothGattCharacteristic control = new BluetoothGattCharacteristic(
                Protocol.CONTROL,
                BluetoothGattCharacteristic.PROPERTY_WRITE | BluetoothGattCharacteristic.PROPERTY_WRITE_NO_RESPONSE,
                BluetoothGattCharacteristic.PERMISSION_WRITE);
        statusChar = new BluetoothGattCharacteristic(
                Protocol.STATUS,
                BluetoothGattCharacteristic.PROPERTY_READ | BluetoothGattCharacteristic.PROPERTY_NOTIFY,
                BluetoothGattCharacteristic.PERMISSION_READ);
        service.addCharacteristic(control);
        service.addCharacteristic(statusChar);
        gatt.addService(service);

        advertiser = adapter.getBluetoothLeAdvertiser();
        if (advertiser == null) {
            setState("This adapter cannot advertise");
            return;
        }
        AdvertiseSettings settings = new AdvertiseSettings.Builder()
                .setAdvertiseMode(AdvertiseSettings.ADVERTISE_MODE_LOW_LATENCY)
                .setTxPowerLevel(AdvertiseSettings.ADVERTISE_TX_POWER_HIGH)
                .setConnectable(true)
                .build();
        AdvertiseData data = new AdvertiseData.Builder()
                .setIncludeDeviceName(false)
                .addServiceUuid(new ParcelUuid(Protocol.SERVICE))
                .addManufacturerData(Protocol.COMPANY_ID, Protocol.payload(Protocol.FLAG_ARMED, 0, null))
                .build();
        advertiser.startAdvertising(settings, data, advertiseCallback);

        p2p = (WifiP2pManager) getSystemService(WIFI_P2P_SERVICE);
        channel = p2p.initialize(this, getMainLooper(), () -> setState("Wi-Fi Direct channel closed"));
    }

    private void join(Protocol.Credential cred) {
        if (p2p == null || channel == null) {
            setState("Wi-Fi Direct is not available");
            return;
        }
        WifiP2pConfig config = new WifiP2pConfig.Builder()
                .setNetworkName(cred.ssid)
                .setPassphrase(cred.passphrase)
                .build();
        config.groupOwnerIntent = 0;
        p2p.connect(channel, config, new WifiP2pManager.ActionListener() {
            @Override public void onSuccess() {
                setState("Connecting to " + cred.ssid);
                watchGroup();
            }
            @Override public void onFailure(int reason) {
                setState("Wi-Fi Direct connect failed (" + reason + ")");
            }
        });
    }

    private void watchGroup() {
        p2p.requestGroupInfo(channel, group -> {
            if (group == null) {
                setState("Waiting for the group");
                return;
            }
            StringBuilder sb = new StringBuilder("In group ").append(group.getNetworkName());
            if (group.isGroupOwner()) sb.append(" (this phone is the owner, which is wrong)");
            setState(sb.toString());
            publishStatus(group);
        });
    }

    private void publishStatus(WifiP2pGroup group) {
        if (statusChar == null) return;
        String text = group.getNetworkName() == null ? "" : group.getNetworkName();
        statusChar.setValue(text.getBytes(StandardCharsets.UTF_8)); // API 33 deprecates this; min is 30
    }

    private void setState(String s) {
        state = s;
        Log.i(TAG, s);
        NotificationManager nm = (NotificationManager) getSystemService(NOTIFICATION_SERVICE);
        nm.notify(NOTE_ID, notification(s));
        sendBroadcast(new Intent("dev.breadway.breadpair.STATE").putExtra("state", s).setPackage(getPackageName()));
    }

    private void channel() {
        NotificationManager nm = (NotificationManager) getSystemService(NOTIFICATION_SERVICE);
        NotificationChannel c = new NotificationChannel(CHANNEL, getString(R.string.channel_name), NotificationManager.IMPORTANCE_LOW);
        c.setDescription(getString(R.string.channel_desc));
        nm.createNotificationChannel(c);
    }

    private Notification notification(String text) {
        PendingIntent stop = PendingIntent.getService(this, 0,
                new Intent(this, CompanionService.class).setAction(ACTION_STOP),
                PendingIntent.FLAG_IMMUTABLE);
        return new Notification.Builder(this, CHANNEL)
                .setSmallIcon(R.mipmap.ic_launcher)
                .setContentTitle(getString(R.string.notification_title))
                .setContentText(text)
                .setOngoing(true)
                .addAction(new Notification.Action.Builder(null, "Stop", stop).build())
                .build();
    }

    @Override
    public void onDestroy() {
        if (advertiser != null) {
            try { advertiser.stopAdvertising(advertiseCallback); } catch (RuntimeException ignored) {}
        }
        if (gatt != null) gatt.close();
        if (p2p != null && channel != null) {
            p2p.removeGroup(channel, null);
            p2p.cancelConnect(channel, null);
        }
        if (wakeLock != null && wakeLock.isHeld()) wakeLock.release();
        super.onDestroy();
    }

    @Override
    public IBinder onBind(Intent intent) { return null; }
}
