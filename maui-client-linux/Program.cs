using System.Runtime.Versioning;
using Microsoft.Maui.Hosting;
using Microsoft.Maui.Platforms.Linux.Gtk4.Platform;

// The Linux head only ever runs on Linux. This silences CA1416 on the GTK API
// calls below (and matches the assembly-level annotation used by the upstream
// Microsoft.Maui.Platforms.Linux.Gtk4.Essentials project).
[assembly: SupportedOSPlatform("linux")]

namespace BridgeToFreedom.Linux;

/// <summary>
/// GTK entry point for the Linux head. The shared App / MainPage / services
/// come from the BridgeToFreedom project — Linux is just another head, like
/// Android or iOS.
/// </summary>
public sealed class Program : GtkMauiApplication
{
    protected override MauiApp CreateMauiApp() => MauiProgram.CreateMauiApp();

    public static void Main(string[] args)
    {
        var app = new Program();
        app.Run(args);
    }
}
