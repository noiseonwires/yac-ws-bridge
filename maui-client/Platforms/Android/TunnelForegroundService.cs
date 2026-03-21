using Android.App;
using Android.Content;
using Android.OS;
using BridgeToFreedom.Services;

namespace BridgeToFreedom.Platforms.Android;

/// <summary>
/// Android foreground service that runs the tunnel and keeps it alive when app is backgrounded.
/// Acquires a partial wake lock to prevent CPU from sleeping.
/// </summary>
[Service(ForegroundServiceType = global::Android.Content.PM.ForegroundService.TypeDataSync)]
public class TunnelForegroundService : Service
{
    private const int NotificationId = 1;
    private const string ChannelId = "btf_tunnel_channel";
    private PowerManager.WakeLock? _wakeLock;
    private Task? _tunnelTask;

    /// <summary>
    /// Static reference to the TunnelService singleton (set by MainPage before starting the service).
    /// Android services don't go through MAUI DI, so we pass it this way.
    /// </summary>
    public static TunnelService? Tunnel { get; set; }

    public override IBinder? OnBind(Intent? intent) => null;

    public override StartCommandResult OnStartCommand(Intent? intent, StartCommandFlags flags, int startId)
    {
        CreateNotificationChannel();

        var notificationIntent = new Intent(this, typeof(MainActivity));
        var pendingIntent = PendingIntent.GetActivity(this, 0, notificationIntent,
            PendingIntentFlags.Immutable);

        var notification = new Notification.Builder(this, ChannelId)
            .SetContentTitle("Bridge to Freedom")
            .SetContentText("Tunnel is active")
            .SetSmallIcon(global::Android.Resource.Drawable.IcDialogInfo)
            .SetContentIntent(pendingIntent)
            .SetOngoing(true)
            .Build();

        StartForeground(NotificationId, notification);

        // Acquire partial wake lock to keep CPU running
        var pm = (PowerManager?)GetSystemService(PowerService);
        _wakeLock = pm?.NewWakeLock(WakeLockFlags.Partial, "BridgeToFreedom::TunnelWakeLock");
        _wakeLock?.Acquire();

        // Start the tunnel inside the service
        if (Tunnel != null && !Tunnel.IsRunning)
        {
            _tunnelTask = Task.Run(async () =>
            {
                try { await Tunnel.StartAsync(); }
                catch (Exception ex) { System.Diagnostics.Debug.WriteLine($"Tunnel fatal: {ex}"); }
            });
        }

        return StartCommandResult.Sticky;
    }

    public override void OnDestroy()
    {
        Tunnel?.Stop();
        // Don't Wait on main thread — just fire and forget.
        // The tunnel will clean up asynchronously.
        _tunnelTask = null;

        try { _wakeLock?.Release(); } catch { }
        _wakeLock = null;

        base.OnDestroy();
    }

    private void CreateNotificationChannel()
    {
        var channel = new NotificationChannel(ChannelId, "Tunnel Service",
            NotificationImportance.Low)
        {
            Description = "Keeps the TCP tunnel running in background"
        };
        var manager = (NotificationManager?)GetSystemService(NotificationService);
        manager?.CreateNotificationChannel(channel);
    }
}
