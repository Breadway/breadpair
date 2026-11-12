package dev.breadway.breadpair;

import android.Manifest;
import android.app.Activity;
import android.bluetooth.BluetoothAdapter;
import android.bluetooth.BluetoothManager;
import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;
import android.content.IntentFilter;
import android.content.pm.PackageManager;
import android.net.Uri;
import android.os.Build;
import android.os.Bundle;
import android.os.PowerManager;
import android.provider.Settings;
import android.widget.Button;
import android.widget.TextView;

/**
 * One screen: the current state, Arm, Stop, and the battery exemption.
 * Everything that has to survive the screen turning off lives in
 * CompanionService.
 */
public class MainActivity extends Activity {
    private static final int PERMISSIONS = 1;

    private TextView status;
    private TextView detail;
    private final BroadcastReceiver stateReceiver = new BroadcastReceiver() {
        @Override public void onReceive(Context context, Intent intent) {
            String s = intent.getStringExtra("state");
            if (s != null) status.setText(s);
        }
    };

    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        setContentView(R.layout.activity_main);
        status = findViewById(R.id.status);
        detail = findViewById(R.id.detail);
        Button arm = findViewById(R.id.arm);
        Button stop = findViewById(R.id.stop);
        Button battery = findViewById(R.id.battery);
        arm.setOnClickListener(v -> arm());
        // Opening the app is enough. The service is what matters, and the
        // button is easy to miss when the screen is driven remotely.
        if (hasPermissions()) startForegroundService(new Intent(this, CompanionService.class));
        stop.setOnClickListener(v -> {
            stopService(new Intent(this, CompanionService.class));
            status.setText("Stopped");
        });
        battery.setOnClickListener(v -> requestBatteryExemption());
    }

    @Override
    protected void onResume() {
        super.onResume();
        registerReceiver(stateReceiver, new IntentFilter("dev.breadway.breadpair.STATE"), RECEIVER_NOT_EXPORTED);
        renderChecklist();
    }

    @Override
    protected void onPause() {
        super.onPause();
        unregisterReceiver(stateReceiver);
    }

    private void arm() {
        if (!hasPermissions()) {
            requestPermissions(needed(), PERMISSIONS);
            return;
        }
        BluetoothManager bm = (BluetoothManager) getSystemService(BLUETOOTH_SERVICE);
        BluetoothAdapter adapter = bm.getAdapter();
        if (adapter == null || !adapter.isEnabled()) {
            status.setText("Turn Bluetooth on first");
            return;
        }
        startForegroundService(new Intent(this, CompanionService.class));
        status.setText("Arming");
    }

    private void renderChecklist() {
        StringBuilder sb = new StringBuilder();
        sb.append(hasPermissions() ? "Permissions granted.\n" : "Permissions missing. Tap Arm to grant them.\n");
        PowerManager pm = (PowerManager) getSystemService(POWER_SERVICE);
        sb.append(pm.isIgnoringBatteryOptimizations(getPackageName())
                ? "Battery use is unrestricted.\n"
                : "Battery optimisation is still on. Tap Allow background use, or the phone will freeze this app while it sleeps.\n");
        sb.append("Wireless debugging has to be switched on, and the laptop paired once, before scrcpy can connect.");
        detail.setText(sb.toString());
    }

    private void requestBatteryExemption() {
        PowerManager pm = (PowerManager) getSystemService(POWER_SERVICE);
        if (pm.isIgnoringBatteryOptimizations(getPackageName())) {
            status.setText("Already unrestricted");
            return;
        }
        Intent intent = new Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS,
                Uri.parse("package:" + getPackageName()));
        startActivity(intent);
    }

    private boolean hasPermissions() {
        for (String p : needed()) {
            if (checkSelfPermission(p) != PackageManager.PERMISSION_GRANTED) return false;
        }
        return true;
    }

    private String[] needed() {
        if (Build.VERSION.SDK_INT >= 33) {
            return new String[]{
                    Manifest.permission.BLUETOOTH_ADVERTISE,
                    Manifest.permission.BLUETOOTH_CONNECT,
                    Manifest.permission.NEARBY_WIFI_DEVICES,
                    Manifest.permission.POST_NOTIFICATIONS,
                    Manifest.permission.ACCESS_FINE_LOCATION
            };
        }
        return new String[]{
                Manifest.permission.BLUETOOTH_ADVERTISE,
                Manifest.permission.BLUETOOTH_CONNECT,
                Manifest.permission.ACCESS_FINE_LOCATION
        };
    }

    @Override
    public void onRequestPermissionsResult(int requestCode, String[] permissions, int[] results) {
        super.onRequestPermissionsResult(requestCode, permissions, results);
        renderChecklist();
        if (hasPermissions()) arm();
    }
}
