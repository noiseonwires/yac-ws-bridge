using BridgeToFreedom.Services;
using System.Text;
using System.Web;

namespace BridgeToFreedom;

public partial class MainPage : ContentPage
{
    private const string PrefAllowedPackages = "AllowedPackages";

    private readonly TunnelService _tunnel;
    private readonly StringBuilder _logBuffer = new();
    private bool _isRunning;
    private HashSet<string> _allowedPackages = new(StringComparer.Ordinal);

    public bool IsNotRunning => !_isRunning;

    public MainPage(TunnelService tunnel)
    {
        InitializeComponent();
        BindingContext = this;
        _tunnel = tunnel;
        _tunnel.OnLog += OnTunnelLog;

        BridgeUrlEntry.Text       = Preferences.Default.Get("BridgeUrl", "wss://");
        AuthTokenEntry.Text       = Preferences.Default.Get("AuthToken", "");
        TunnelAddressEntry.Text   = Preferences.Default.Get("TunnelAddress", "10.200.0.2");
        PeerAddressEntry.Text     = Preferences.Default.Get("PeerAddress", "10.200.0.1");
        MtuEntry.Text             = Preferences.Default.Get("Mtu", "1400");
        RelaySwitch.IsToggled     = Preferences.Default.Get("Relay", false);
        _allowedPackages          = LoadAllowedPackages();
        UpdateSelectAppsButton();

        if (_tunnel.IsRunning)
        {
            _isRunning = true;
            ConnectButton.Text = "DISCONNECT";
            ConnectButton.BackgroundColor = Color.FromArgb("#D32F2F");
            OnPropertyChanged(nameof(IsNotRunning));
            _tunnel.OnStopped += OnTunnelStopped;
            AddLog("[resumed — VPN is running in background]");
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

            var bridgeUri = new Uri(url);
            var qs = HttpUtility.ParseQueryString("");
            qs["token"] = AuthTokenEntry.Text?.Trim() ?? "";
            qs["addr"]  = TunnelAddressEntry.Text?.Trim() ?? "";
            qs["peer"]  = PeerAddressEntry.Text?.Trim() ?? "";
            qs["mtu"]   = MtuEntry.Text?.Trim() ?? "";
            if (RelaySwitch.IsToggled) qs["relay"] = "1";

            var btfUrl = $"btf://{bridgeUri.Host}{bridgeUri.AbsolutePath}?{qs}";
            await Clipboard.Default.SetTextAsync(btfUrl);
            AddLog("Config exported to clipboard");
        }
        catch (Exception ex) { AddLog($"Export failed: {ex.Message}"); }
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

            var uri = new Uri(text);
            var qs = HttpUtility.ParseQueryString(uri.Query);

            BridgeUrlEntry.Text     = $"wss://{uri.Host}{uri.AbsolutePath}";
            AuthTokenEntry.Text     = qs["token"] ?? "";
            TunnelAddressEntry.Text = qs["addr"] ?? TunnelAddressEntry.Text;
            PeerAddressEntry.Text   = qs["peer"] ?? PeerAddressEntry.Text;
            MtuEntry.Text           = qs["mtu"]  ?? MtuEntry.Text;
            RelaySwitch.IsToggled   = qs["relay"] == "1";
            AddLog("Config imported from clipboard");
        }
        catch (Exception ex) { AddLog($"Import failed: {ex.Message}"); }
    }

    private async void OnConnectClicked(object? sender, EventArgs e)
    {
        if (_isRunning)
        {
            StopPlatformService();
            _tunnel.Stop();
            SetStoppedUi();
            AddLog("Stopped by user.");
            return;
        }

        var url = BridgeUrlEntry.Text?.Trim();
        var token = AuthTokenEntry.Text?.Trim();
        var addr = TunnelAddressEntry.Text?.Trim();
        var peer = PeerAddressEntry.Text?.Trim();
        var mtuStr = MtuEntry.Text?.Trim();

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
        if (string.IsNullOrEmpty(addr) || !System.Net.IPAddress.TryParse(addr, out _))
        {
            await DisplayAlertAsync("Error", "Local tunnel IP is invalid", "OK");
            return;
        }
        if (string.IsNullOrEmpty(peer) || !System.Net.IPAddress.TryParse(peer, out _))
        {
            await DisplayAlertAsync("Error", "Peer tunnel IP is invalid", "OK");
            return;
        }
        if (!int.TryParse(mtuStr, out var mtu) || mtu < 576 || mtu > 9000)
        {
            await DisplayAlertAsync("Error", "MTU must be 576-9000", "OK");
            return;
        }

        // Ask for VPN permission before configuring anything
        var granted = await MainActivity.RequestVpnPermissionAsync();
        if (!granted)
        {
            await DisplayAlertAsync("Permission", "VPN permission was not granted.", "OK");
            return;
        }

        _tunnel.BridgeUrl = url;
        _tunnel.AuthToken = token;
        _tunnel.Relay     = RelaySwitch.IsToggled;

        Preferences.Default.Set("BridgeUrl", url);
        Preferences.Default.Set("AuthToken", token);
        Preferences.Default.Set("TunnelAddress", addr);
        Preferences.Default.Set("PeerAddress", peer);
        Preferences.Default.Set("Mtu", mtuStr!);
        Preferences.Default.Set("Relay", RelaySwitch.IsToggled);

        _isRunning = true;
        ConnectButton.Text = "DISCONNECT";
        ConnectButton.BackgroundColor = Color.FromArgb("#D32F2F");
        OnPropertyChanged(nameof(IsNotRunning));

        _logBuffer.Clear();
        LogLabel.Text = "";

        StartPlatformService(addr, peer, mtu);
        _tunnel.OnStopped += OnTunnelStopped;
    }

    private void SetStoppedUi()
    {
        _isRunning = false;
        ConnectButton.Text = "CONNECT";
        ConnectButton.BackgroundColor = Color.FromArgb("#512BD4");
        OnPropertyChanged(nameof(IsNotRunning));
    }

    private void OnTunnelStopped()
    {
        _tunnel.OnStopped -= OnTunnelStopped;
        StopPlatformService();
        MainThread.BeginInvokeOnMainThread(SetStoppedUi);
    }

    private void OnTunnelLog(string line) => MainThread.BeginInvokeOnMainThread(() => AddLog(line));

    private void AddLog(string line)
    {
        _logBuffer.AppendLine(line);
        var lines = _logBuffer.ToString().Split('\n');
        if (lines.Length > 200)
        {
            _logBuffer.Clear();
            foreach (var l in lines[^200..]) _logBuffer.AppendLine(l);
        }
        LogLabel.Text = _logBuffer.ToString();
        try { LogScrollView.ScrollToAsync(LogLabel, ScrollToPosition.End, false); } catch { }
    }

    private void StartPlatformService(string localAddr, string peerAddr, int mtu)
    {
        Platforms.Android.BtfVpnService.Tunnel = _tunnel;
        var context = global::Android.App.Application.Context;
        var intent = new global::Android.Content.Intent(context, typeof(Platforms.Android.BtfVpnService));
        intent.PutExtra(Platforms.Android.BtfVpnService.ExtraTunnelAddress, localAddr);
        intent.PutExtra(Platforms.Android.BtfVpnService.ExtraPeerAddress, peerAddr);
        intent.PutExtra(Platforms.Android.BtfVpnService.ExtraMtu, mtu);
        if (_allowedPackages.Count > 0)
        {
            intent.PutExtra(Platforms.Android.BtfVpnService.ExtraAllowedPackages,
                _allowedPackages.ToArray());
        }
        context.StartForegroundService(intent);
    }

    private static void StopPlatformService()
    {
        var context = global::Android.App.Application.Context;
        var intent = new global::Android.Content.Intent(context, typeof(Platforms.Android.BtfVpnService));
        intent.SetAction(Platforms.Android.BtfVpnService.ActionStop);
        context.StartService(intent);
        Platforms.Android.BtfVpnService.Tunnel = null;
    }

    // -- Per-app picker --------------------------------------------------------

    private async void OnSelectAppsClicked(object? sender, EventArgs e)
    {
        if (_isRunning)
        {
            await DisplayAlertAsync("Disconnect first",
                "Stop the VPN before changing the app list.", "OK");
            return;
        }

        var page = new AppPickerPage(_allowedPackages);
        await Navigation.PushModalAsync(page);
        var result = await page.Result;
        if (result == null) return; // cancelled

        _allowedPackages = result;
        SaveAllowedPackages(_allowedPackages);
        UpdateSelectAppsButton();
        AddLog(_allowedPackages.Count == 0
            ? "App selection cleared — VPN will route ALL apps."
            : $"App selection saved: {_allowedPackages.Count} app(s) will use the VPN.");
    }

    private void UpdateSelectAppsButton()
    {
        SelectAppsButton.Text = _allowedPackages.Count == 0
            ? "Select apps for VPN… (all apps)"
            : $"Select apps for VPN… ({_allowedPackages.Count} selected)";
    }

    private static HashSet<string> LoadAllowedPackages()
    {
        var raw = Preferences.Default.Get(PrefAllowedPackages, "");
        if (string.IsNullOrWhiteSpace(raw)) return new HashSet<string>(StringComparer.Ordinal);
        return new HashSet<string>(
            raw.Split(',', StringSplitOptions.RemoveEmptyEntries | StringSplitOptions.TrimEntries),
            StringComparer.Ordinal);
    }

    private static void SaveAllowedPackages(HashSet<string> packages)
    {
        Preferences.Default.Set(PrefAllowedPackages, string.Join(',', packages));
    }
}
