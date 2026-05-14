using Android.App;
using Android.Content;
using Android.Content.PM;
using Android.Net;
using Android.OS;

namespace BridgeToFreedom;

[Activity(Theme = "@style/Maui.SplashTheme", MainLauncher = true,
    LaunchMode = LaunchMode.SingleTop,
    ConfigurationChanges = ConfigChanges.ScreenSize | ConfigChanges.Orientation
        | ConfigChanges.UiMode | ConfigChanges.ScreenLayout
        | ConfigChanges.SmallestScreenSize | ConfigChanges.Density)]
public class MainActivity : MauiAppCompatActivity
{
    public const int VpnPrepareRequestCode = 0x4242;

    private static TaskCompletionSource<bool>? _vpnPermissionTcs;

    /// <summary>
    /// Asks for VPN permission from the user. Returns true if granted (or
    /// already granted previously).
    /// </summary>
    public static Task<bool> RequestVpnPermissionAsync()
    {
        var ctx = global::Android.App.Application.Context;
        var prepareIntent = VpnService.Prepare(ctx);
        if (prepareIntent == null) return Task.FromResult(true);

        var current = Microsoft.Maui.ApplicationModel.Platform.CurrentActivity;
        if (current == null) return Task.FromResult(false);

        _vpnPermissionTcs?.TrySetResult(false);
        var tcs = new TaskCompletionSource<bool>();
        _vpnPermissionTcs = tcs;
        current.StartActivityForResult(prepareIntent, VpnPrepareRequestCode);
        return tcs.Task;
    }

    protected override void OnActivityResult(int requestCode, Result resultCode, Intent? data)
    {
        if (requestCode == VpnPrepareRequestCode)
        {
            var ok = resultCode == Result.Ok;
            _vpnPermissionTcs?.TrySetResult(ok);
            _vpnPermissionTcs = null;
        }
        base.OnActivityResult(requestCode, resultCode, data);
    }
}
