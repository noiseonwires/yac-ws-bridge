using BridgeToFreedom.Services;
using Microsoft.Extensions.Logging;
using Microsoft.Maui.Hosting;
using Microsoft.Maui.Platforms.Linux.Gtk4.Essentials.Hosting;
using Microsoft.Maui.Platforms.Linux.Gtk4.Hosting;

namespace BridgeToFreedom.Linux;

public static class MauiProgram
{
    public static MauiApp CreateMauiApp()
    {
        AppDomain.CurrentDomain.UnhandledException += (sender, args) =>
        {
            var ex = args.ExceptionObject as Exception;
            System.Diagnostics.Debug.WriteLine($"FATAL: {ex}");
        };

        TaskScheduler.UnobservedTaskException += (sender, args) =>
        {
            System.Diagnostics.Debug.WriteLine($"UnobservedTask: {args.Exception}");
            args.SetObserved();
        };

        var builder = MauiApp.CreateBuilder()
            .UseMauiAppLinuxGtk4<App>()
            // Wires DI + Preferences.SetDefault / Clipboard.SetDefault / etc. to the
            // Linux implementations. Without this, static Essentials APIs throw
            // NotImplementedInReferenceAssemblyException at runtime.
            .AddLinuxGtk4Essentials()
            .ConfigureFonts(fonts =>
            {
                fonts.AddFont("OpenSans-Regular.ttf", "OpenSansRegular");
                fonts.AddFont("OpenSans-Semibold.ttf", "OpenSansSemibold");
            });

        builder.Services.AddSingleton<TunnelService>();
        builder.Services.AddTransient<MainPage>();

#if DEBUG
        builder.Logging.AddDebug();
#endif

        return builder.Build();
    }
}
