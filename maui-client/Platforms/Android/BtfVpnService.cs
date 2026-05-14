using Android.App;
using Android.Content;
using Android.Net;
using Android.OS;
using Android.Runtime;
using BridgeToFreedom.Services;
using Java.IO;

namespace BridgeToFreedom.Platforms.Android;

/// <summary>
/// Android VpnService that creates a TUN interface and bridges IP packets
/// between it and the upstream WebSocket-driven <see cref="TunnelService"/>.
/// </summary>
[Service(
    Permission = "android.permission.BIND_VPN_SERVICE",
    Exported = true,
    ForegroundServiceType = global::Android.Content.PM.ForegroundService.TypeDataSync)]
[IntentFilter(new[] { "android.net.VpnService" })]
public class BtfVpnService : VpnService
{
    public const string ActionStart = "com.btf.helper.START_VPN";
    public const string ActionStop  = "com.btf.helper.STOP_VPN";
    public const string ExtraTunnelAddress   = "tunnelAddress";
    public const string ExtraPeerAddress     = "peerAddress";
    public const string ExtraMtu             = "mtu";
    /// <summary>
    /// String[] of package names. If non-empty, only those apps are routed
    /// through the VPN (plus an automatic disallow for our own package).
    /// If null/empty, all apps go through the VPN (except our own).
    /// </summary>
    public const string ExtraAllowedPackages = "allowedPackages";

    private const int NotificationId = 1;
    private const string ChannelId = "btf_vpn_channel";

    public static TunnelService? Tunnel { get; set; }

    private ParcelFileDescriptor? _tunFd;
    private FileInputStream? _tunIn;
    private FileOutputStream? _tunOut;
    private CancellationTokenSource? _ioCts;
    private PowerManager.WakeLock? _wakeLock;
    private Task? _tunnelTask;
    private Task? _readTask;

    public override IBinder? OnBind(Intent? intent)
    {
        // VpnService binds for the system; otherwise no client binding.
        if (intent?.Action == "android.net.VpnService") return base.OnBind(intent);
        return null;
    }

    public override StartCommandResult OnStartCommand(Intent? intent, StartCommandFlags flags, int startId)
    {
        if (intent?.Action == ActionStop)
        {
            StopTunnel();
            StopSelf();
            return StartCommandResult.NotSticky;
        }

        if (Tunnel == null)
        {
            System.Diagnostics.Debug.WriteLine("BtfVpnService: Tunnel singleton not set; stopping");
            StopSelf();
            return StartCommandResult.NotSticky;
        }

        var localAddr = intent?.GetStringExtra(ExtraTunnelAddress) ?? "10.200.0.2";
        var peerAddr  = intent?.GetStringExtra(ExtraPeerAddress)   ?? "10.200.0.1";
        var mtu       = intent?.GetIntExtra(ExtraMtu, 1400)        ?? 1400;
        var allowed   = intent?.GetStringArrayExtra(ExtraAllowedPackages) ?? Array.Empty<string>();

        CreateNotificationChannel();
        StartForegroundNotification(localAddr);

        var pm = (PowerManager?)GetSystemService(PowerService);
        _wakeLock = pm?.NewWakeLock(WakeLockFlags.Partial, "BridgeToFreedom::VpnWakeLock");
        _wakeLock?.Acquire();

        if (!EstablishTun(localAddr, peerAddr, mtu, allowed))
        {
            StopSelf();
            return StartCommandResult.NotSticky;
        }

        _ioCts = new CancellationTokenSource();
        var tunnel = Tunnel!;
        tunnel.OnInboundPacket += OnInboundPacket;

        // Start the upstream WS pump
        _tunnelTask = Task.Run(async () =>
        {
            try { await tunnel.StartAsync(); }
            catch (Exception ex) { System.Diagnostics.Debug.WriteLine($"Tunnel fatal: {ex}"); }
        });

        // Start the TUN read loop (TUN -> WS)
        _readTask = Task.Run(() => TunReadLoop(_ioCts.Token));

        return StartCommandResult.Sticky;
    }

    private bool EstablishTun(string local, string peer, int mtu, string[] allowedPackages)
    {
        try
        {
            var builder = new Builder(this)
                .SetSession("Bridge to Freedom")
                .SetMtu(mtu)
                .AddAddress(local, 30)
                // Route everything through the VPN. The peer side (Go adapter)
                // is responsible for IP forwarding / NAT.
                .AddRoute("0.0.0.0", 0)
                // Default DNS — use a public resolver since the kernel needs at
                // least one. Users may want to make this configurable later.
                .AddDnsServer("1.1.1.1")
                .AddDnsServer("8.8.8.8");

            // Per-app routing.
            //
            // Android's VpnService allows EITHER an allow-list OR a disallow-
            // list per Builder, not both. So:
            //   - If the user picked specific apps, switch to allow-list mode
            //     and only add those packages. Our own package is implicitly
            //     excluded because it's not on the allow-list.
            //   - Otherwise (no selection), default to disallow-listing only
            //     ourselves so every other app gets routed.
            //
            // Either way the app's own WS traffic never loops through the VPN.
            if (allowedPackages.Length > 0)
            {
                int added = 0;
                foreach (var pkg in allowedPackages)
                {
                    if (string.IsNullOrWhiteSpace(pkg) || pkg == PackageName) continue;
                    try
                    {
                        builder.AddAllowedApplication(pkg);
                        added++;
                    }
                    catch (global::Android.Content.PM.PackageManager.NameNotFoundException)
                    {
                        // App was uninstalled since the user picked it. Skip silently.
                        System.Diagnostics.Debug.WriteLine($"AddAllowedApplication: package not found: {pkg}");
                    }
                }
                if (added == 0)
                {
                    // Every selected package was missing — fall back to default
                    // (disallow self only) instead of producing a TUN that
                    // routes nothing.
                    System.Diagnostics.Debug.WriteLine("All selected packages missing; falling back to disallow-self mode");
                    builder.AddDisallowedApplication(PackageName!);
                }
            }
            else
            {
                builder.AddDisallowedApplication(PackageName!);
            }

            _tunFd = builder.Establish();
            if (_tunFd == null)
            {
                System.Diagnostics.Debug.WriteLine("VpnService.Builder.Establish() returned null");
                return false;
            }
            _tunIn  = new FileInputStream(_tunFd.FileDescriptor);
            _tunOut = new FileOutputStream(_tunFd.FileDescriptor);
            var modeDesc = allowedPackages.Length > 0
                ? $"per-app ({allowedPackages.Length} allowed)"
                : "all apps (self excluded)";
            System.Diagnostics.Debug.WriteLine($"TUN established local={local}/30 peer={peer} mtu={mtu} routing={modeDesc}");
            return true;
        }
        catch (Exception ex)
        {
            System.Diagnostics.Debug.WriteLine($"EstablishTun failed: {ex}");
            return false;
        }
    }

    private void OnInboundPacket(byte[] packet)
    {
        var fout = _tunOut;
        if (fout == null) return;
        try { fout.Write(packet); }
        catch (Exception ex) { System.Diagnostics.Debug.WriteLine($"TUN write: {ex.Message}"); }
    }

    private void TunReadLoop(CancellationToken ct)
    {
        var fin = _tunIn;
        var tunnel = Tunnel;
        if (fin == null || tunnel == null) return;

        tunnel.LogExternal("TunReadLoop started");
        long totalRead = 0;
        long zeroReads = 0;
        long batchesSent = 0;
        long singlesSent = 0;
        var lastStatsTick = System.Environment.TickCount;

        // Each TUN read returns exactly one IP packet. 64 KiB is the upper
        // bound for IPv4; we never expect packets larger than the MTU but
        // size the buffer generously.
        var buf = new byte[64 * 1024];

        // Outbound coalescer: when the TUN FD has more than one packet
        // immediately available, drain a small burst and send them together
        // in a single PACKET_BATCH frame. We cap both packet count and total
        // payload bytes so a single WS frame stays reasonable. The wire
        // protocol's batch encoding adds 2 bytes of length prefix per packet
        // plus 1 byte of message type for the whole frame.
        //
        // BatchLingerMs lets us coalesce packets that arrive within a short
        // time window (not just the ones already buffered when we wake up).
        // A tiny value (1–2 ms) is enough to merge bursts like back-to-back
        // TCP segments without noticeably hurting interactive latency.
        const int MaxBatchPackets = 32;
        const int MaxBatchBytes = 60 * 1024;
        const int BatchLingerMs = 2;
        var batch = new List<byte[]>(MaxBatchPackets);

        while (!ct.IsCancellationRequested)
        {
            int n;
            try { n = fin.Read(buf); }
            catch (Exception ex)
            {
                tunnel.LogExternal($"TunReadLoop exit on read error: {ex.GetType().Name}: {ex.Message} (totalRead={totalRead})");
                return;
            }
            if (n < 0)
            {
                tunnel.LogExternal($"TunReadLoop exit: EOF (totalRead={totalRead})");
                return;
            }
            if (n == 0)
            {
                // The TUN FD is non-blocking on Android: read() returns 0 when
                // no packet is currently waiting. Sleep briefly and retry to
                // avoid both busy-looping and exiting prematurely.
                zeroReads++;
                try { Thread.Sleep(10); } catch { }
                var now0 = System.Environment.TickCount;
                if (now0 - lastStatsTick > 5000)
                {
                    lastStatsTick = now0;
                    tunnel.LogExternal($"TunReadLoop alive: totalRead={totalRead} zeroReads={zeroReads} batches={batchesSent} singles={singlesSent}");
                }
                continue;
            }
            totalRead++;

            // First packet of a potential batch.
            var first = new byte[n];
            Buffer.BlockCopy(buf, 0, first, 0, n);
            batch.Add(first);
            int batchBytes = n;

            // Opportunistically drain more packets. First, take everything
            // that's already buffered (non-blocking reads returning >0). If
            // we still have room and the batch is short, linger briefly to
            // catch packets that arrive within BatchLingerMs — useful for
            // bursts like back-to-back TCP segments where the second packet
            // hits the FD a fraction of a millisecond after the first.
            var lingerDeadline = System.Environment.TickCount + BatchLingerMs;
            while (batch.Count < MaxBatchPackets && batchBytes < MaxBatchBytes)
            {
                int m;
                try { m = fin.Read(buf); }
                catch { m = 0; }
                if (m > 0)
                {
                    totalRead++;
                    var extra = new byte[m];
                    Buffer.BlockCopy(buf, 0, extra, 0, m);
                    batch.Add(extra);
                    batchBytes += m;
                    continue;
                }
                // m <= 0: nothing buffered right now. Check linger budget.
                if (System.Environment.TickCount >= lingerDeadline) break;
                try { Thread.Sleep(1); } catch { break; }
            }

            // Fire-and-forget send: the tunnel handles its own back-pressure
            // and reconnect; if the peer is gone we silently drop and rely on
            // TCP retransmission to recover.
            if (batch.Count == 1)
            {
                singlesSent++;
                _ = tunnel.SendPacketAsync(batch[0], ct);
            }
            else
            {
                batchesSent++;
                // Snapshot the list because we're about to clear it for reuse
                // and SendPacketBatchAsync runs asynchronously.
                _ = tunnel.SendPacketBatchAsync(batch.ToArray(), ct);
            }
            batch.Clear();

            var now = System.Environment.TickCount;
            if (now - lastStatsTick > 5000)
            {
                lastStatsTick = now;
                tunnel.LogExternal($"TunReadLoop alive: totalRead={totalRead} zeroReads={zeroReads} batches={batchesSent} singles={singlesSent}");
            }
        }
        tunnel.LogExternal($"TunReadLoop exit on cancel (totalRead={totalRead} zeroReads={zeroReads} batches={batchesSent} singles={singlesSent})");
    }

    public override void OnDestroy()
    {
        StopTunnel();
        try { _wakeLock?.Release(); } catch { }
        _wakeLock = null;
        base.OnDestroy();
    }

    private void StopTunnel()
    {
        try { _ioCts?.Cancel(); } catch { }

        var tunnel = Tunnel;
        if (tunnel != null)
        {
            tunnel.OnInboundPacket -= OnInboundPacket;
            tunnel.Stop();
        }

        try { _tunIn?.Close(); } catch { }
        try { _tunOut?.Close(); } catch { }
        try { _tunFd?.Close(); } catch { }
        _tunIn = null;
        _tunOut = null;
        _tunFd = null;
    }

    private void StartForegroundNotification(string localAddr)
    {
        var notificationIntent = new Intent(this, typeof(MainActivity));
        var pendingIntent = PendingIntent.GetActivity(this, 0, notificationIntent,
            PendingIntentFlags.Immutable);

        var notification = new Notification.Builder(this, ChannelId)
            .SetContentTitle("Bridge to Freedom")
            .SetContentText($"VPN active ({localAddr})")
            .SetSmallIcon(global::Android.Resource.Drawable.IcDialogInfo)
            .SetContentIntent(pendingIntent)
            .SetOngoing(true)
            .Build();

        StartForeground(NotificationId, notification);
    }

    private void CreateNotificationChannel()
    {
        var channel = new NotificationChannel(ChannelId, "VPN Tunnel",
            NotificationImportance.Low)
        {
            Description = "Keeps the IP tunnel running in background"
        };
        var manager = (NotificationManager?)GetSystemService(NotificationService);
        manager?.CreateNotificationChannel(channel);
    }
}
