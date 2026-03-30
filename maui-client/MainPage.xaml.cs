using BridgeToFreedom.Services;
using System.Text;
using System.Web;

namespace BridgeToFreedom;

public partial class MainPage : ContentPage
{
    private readonly TunnelService _tunnel;
    private readonly StringBuilder _logBuffer = new();
    private bool _isRunning;

    public bool IsNotRunning => !_isRunning;

    public MainPage(TunnelService tunnel)
    {
        InitializeComponent();
        BindingContext = this;
        _tunnel = tunnel;
        _tunnel.OnLog += OnTunnelLog;

        // Load saved settings
        BridgeUrlEntry.Text = Preferences.Default.Get("BridgeUrl", "wss://");
        AuthTokenEntry.Text = Preferences.Default.Get("AuthToken", "");
        ListenAddressEntry.Text = Preferences.Default.Get("ListenAddress", "0.0.0.0");
        ListenPortEntry.Text = Preferences.Default.Get("ListenPort", "5080");
        RelaySwitch.IsToggled = Preferences.Default.Get("Relay", false);
        CoalesceSwitch.IsToggled = Preferences.Default.Get("WriteCoalescing", false);

        // Restore UI state if tunnel is already running (e.g. after activity recreate from background)
        if (_tunnel.IsRunning)
        {
            _isRunning = true;
            ConnectButton.Text = "DISCONNECT";
            ConnectButton.BackgroundColor = Color.FromArgb("#D32F2F");
            OnPropertyChanged(nameof(IsNotRunning));
            _tunnel.OnStopped += OnTunnelStopped;
            AddLog("[resumed — tunnel is running in background]");
        }
    }

    private async void OnExportClicked(object? sender, EventArgs e)
    {
        try
        {
            var url = BridgeUrlEntry.Text?.Trim() ?? "";
            if (!url.StartsWith("wss://"))
            {
                await DisplayAlertAsync("Error", "Bridge URL must start with wss://", "OK");
                return;
            }

            // btf://host/path?token=X&listen=addr:port&relay=1
            var bridgeUri = new Uri(url);
            var qs = HttpUtility.ParseQueryString("");
            qs["token"] = AuthTokenEntry.Text?.Trim() ?? "";
            qs["listen"] = $"{ListenAddressEntry.Text?.Trim()}:{ListenPortEntry.Text?.Trim()}";
            if (RelaySwitch.IsToggled) qs["relay"] = "1";
            if (CoalesceSwitch.IsToggled) qs["coalesce"] = "1";

            var btfUrl = $"btf://{bridgeUri.Host}{bridgeUri.AbsolutePath}?{qs}";
            await Clipboard.Default.SetTextAsync(btfUrl);
            AddLog($"Config exported to clipboard");
        }
        catch (Exception ex)
        {
            AddLog($"Export failed: {ex.Message}");
        }
    }

    private async void OnImportClicked(object? sender, EventArgs e)
    {
        try
        {
            var text = await Clipboard.Default.GetTextAsync();
            if (string.IsNullOrWhiteSpace(text) || !text.StartsWith("btf://"))
            {
                await DisplayAlertAsync("Import", "No btf:// URL found in clipboard", "OK");
                return;
            }

            // btf://host/path?token=X&listen=addr:port&relay=1  ->  wss://host/path
            var uri = new Uri(text);
            var qs = HttpUtility.ParseQueryString(uri.Query);

            BridgeUrlEntry.Text = $"wss://{uri.Host}{uri.AbsolutePath}";
            AuthTokenEntry.Text = qs["token"] ?? "";

            var listen = qs["listen"] ?? "";
            var colonIdx = listen.LastIndexOf(':');
            if (colonIdx > 0)
            {
                ListenAddressEntry.Text = listen[..colonIdx];
                ListenPortEntry.Text = listen[(colonIdx + 1)..];
            }

            RelaySwitch.IsToggled = qs["relay"] == "1";
            CoalesceSwitch.IsToggled = qs["coalesce"] == "1";
            AddLog("Config imported from clipboard");
        }
        catch (Exception ex)
        {
            AddLog($"Import failed: {ex.Message}");
        }
    }

    private async void OnConnectClicked(object? sender, EventArgs e)
    {
        if (_isRunning)
        {
            // Stop — this triggers OnDestroy in the service which calls Tunnel.Stop()
            StopPlatformService();
            _tunnel.Stop();
            _isRunning = false;
            ConnectButton.Text = "CONNECT";
            ConnectButton.BackgroundColor = Color.FromArgb("#512BD4");
            OnPropertyChanged(nameof(IsNotRunning));
            AddLog("Stopped by user.");
            return;
        }

        // Validate
        var url = BridgeUrlEntry.Text?.Trim();
        var token = AuthTokenEntry.Text?.Trim();
        var addr = ListenAddressEntry.Text?.Trim();
        var portStr = ListenPortEntry.Text?.Trim();

        if (string.IsNullOrEmpty(url) || !url.StartsWith("wss://"))
        {
            await DisplayAlertAsync("Error", "Bridge URL must start with wss://", "OK");
            return;
        }
        if (string.IsNullOrEmpty(token))
        {
            await DisplayAlertAsync("Error", "Auth token is required", "OK");
            return;
        }
        if (!int.TryParse(portStr, out var port) || port < 1 || port > 65535)
        {
            await DisplayAlertAsync("Error", "Port must be 1-65535", "OK");
            return;
        }

        _tunnel.BridgeUrl = url;
        _tunnel.AuthToken = token;
        _tunnel.ListenAddress = addr ?? "0.0.0.0";
        _tunnel.ListenPort = port;
        _tunnel.Relay = RelaySwitch.IsToggled;
        _tunnel.WriteCoalescing = CoalesceSwitch.IsToggled;

        // Save settings
        Preferences.Default.Set("BridgeUrl", url);
        Preferences.Default.Set("AuthToken", token);
        Preferences.Default.Set("ListenAddress", addr ?? "0.0.0.0");
        Preferences.Default.Set("ListenPort", portStr!);
        Preferences.Default.Set("Relay", RelaySwitch.IsToggled);
        Preferences.Default.Set("WriteCoalescing", CoalesceSwitch.IsToggled);

        _isRunning = true;
        ConnectButton.Text = "DISCONNECT";
        ConnectButton.BackgroundColor = Color.FromArgb("#D32F2F");
        OnPropertyChanged(nameof(IsNotRunning));

        _logBuffer.Clear();
        LogLabel.Text = "";

        // Start: on Android the foreground service runs the tunnel;
        // on other platforms we run it in a Task.
        StartPlatformService();

        _tunnel.OnStopped += OnTunnelStopped;
    }

    private void OnTunnelStopped()
    {
        _tunnel.OnStopped -= OnTunnelStopped;
        StopPlatformService();
        MainThread.BeginInvokeOnMainThread(() =>
        {
            _isRunning = false;
            ConnectButton.Text = "CONNECT";
            ConnectButton.BackgroundColor = Color.FromArgb("#512BD4");
            OnPropertyChanged(nameof(IsNotRunning));
        });
    }

    private void OnTunnelLog(string line)
    {
        MainThread.BeginInvokeOnMainThread(() => AddLog(line));
    }

    private void AddLog(string line)
    {
        _logBuffer.AppendLine(line);
        // Keep last 200 lines
        var lines = _logBuffer.ToString().Split('\n');
        if (lines.Length > 200)
        {
            _logBuffer.Clear();
            foreach (var l in lines[^200..])
                _logBuffer.AppendLine(l);
        }
        LogLabel.Text = _logBuffer.ToString();
        try { LogScrollView.ScrollToAsync(LogLabel, ScrollToPosition.End, false); }
        catch { }
    }

    private void StartPlatformService()
    {
#if ANDROID
        Platforms.Android.TunnelForegroundService.Tunnel = _tunnel;
        var context = Android.App.Application.Context;
        var intent = new Android.Content.Intent(context, typeof(Platforms.Android.TunnelForegroundService));
        context.StartForegroundService(intent);
#else
        // iOS, macOS, Windows: run the tunnel in a background task.
        // iOS stays alive via beginBackgroundTask + BGProcessingTask in AppDelegate.
        _ = Task.Run(async () =>
        {
            try { await _tunnel.StartAsync(); }
            catch (Exception ex) { AddLog($"Fatal: {ex.Message}"); }
            finally { OnTunnelStopped(); }
        });
#endif
    }

    private static void StopPlatformService()
    {
#if ANDROID
        var context = Android.App.Application.Context;
        var intent = new Android.Content.Intent(context, typeof(Platforms.Android.TunnelForegroundService));
        context.StopService(intent);
        Platforms.Android.TunnelForegroundService.Tunnel = null;
#endif
        // iOS/macOS/Windows: tunnel stops via _tunnel.Stop() called before this
    }
}
