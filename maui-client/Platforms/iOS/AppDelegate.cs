using Foundation;
using UIKit;
using BackgroundTasks;

namespace BridgeToFreedom;

[Register("AppDelegate")]
public class AppDelegate : MauiUIApplicationDelegate
{
    private const string BgTaskId = "com.btf.helper.tunnel";
    private nint _backgroundTaskId = UIApplication.BackgroundTaskInvalid;

    protected override MauiApp CreateMauiApp() => MauiProgram.CreateMauiApp();

    public override bool FinishedLaunching(UIApplication application, NSDictionary? launchOptions)
    {
        // Register BGProcessingTask for long-running background work
        BGTaskScheduler.Shared.Register(BgTaskId, null, task =>
        {
            HandleBgTask((BGProcessingTask)task);
        });

        return base.FinishedLaunching(application, launchOptions);
    }

    public override void DidEnterBackground(UIApplication application)
    {
        base.DidEnterBackground(application);

#if IOS_BACKGROUND_AUDIO
        // Start silent audio to keep the app alive indefinitely
        Platforms.iOS.SilentAudioService.Start();
#endif

        // Begin a background task to keep the tunnel alive
        _backgroundTaskId = application.BeginBackgroundTask("TunnelBackground", () =>
        {
            // Expiration handler — iOS is about to kill us
            System.Diagnostics.Debug.WriteLine("[iOS] Background task expiring");
            if (_backgroundTaskId != UIApplication.BackgroundTaskInvalid)
            {
                application.EndBackgroundTask(_backgroundTaskId);
                _backgroundTaskId = UIApplication.BackgroundTaskInvalid;
            }
        });

        System.Diagnostics.Debug.WriteLine($"[iOS] Background task started: {_backgroundTaskId}");

        // Schedule a BGProcessingTask for extended background time
        ScheduleBgTask();
    }

    public override void WillEnterForeground(UIApplication application)
    {
        base.WillEnterForeground(application);

#if IOS_BACKGROUND_AUDIO
        Platforms.iOS.SilentAudioService.Stop();
#endif

        // End the background task if we had one
        if (_backgroundTaskId != UIApplication.BackgroundTaskInvalid)
        {
            application.EndBackgroundTask(_backgroundTaskId);
            _backgroundTaskId = UIApplication.BackgroundTaskInvalid;
            System.Diagnostics.Debug.WriteLine("[iOS] Background task ended (foreground)");
        }
    }

    private void ScheduleBgTask()
    {
        var request = new BGProcessingTaskRequest(BgTaskId)
        {
            RequiresNetworkConnectivity = true,
            RequiresExternalPower = false,
        };
        try
        {
            BGTaskScheduler.Shared.Submit(request, out var error);
            if (error != null)
                System.Diagnostics.Debug.WriteLine($"[iOS] BGTask schedule error: {error}");
            else
                System.Diagnostics.Debug.WriteLine("[iOS] BGTask scheduled");
        }
        catch (Exception ex)
        {
            System.Diagnostics.Debug.WriteLine($"[iOS] BGTask schedule exception: {ex.Message}");
        }
    }

    private void HandleBgTask(BGProcessingTask task)
    {
        System.Diagnostics.Debug.WriteLine("[iOS] BGProcessingTask executing");

        // The tunnel is already running (TunnelService singleton).
        // Just schedule the next task and mark complete.
        ScheduleBgTask();

        task.ExpirationHandler = () =>
        {
            System.Diagnostics.Debug.WriteLine("[iOS] BGProcessingTask expired");
            task.SetTaskCompleted(false);
        };

        // Keep it alive for a bit then complete
        Task.Delay(25000).ContinueWith(_ =>
        {
            task.SetTaskCompleted(true);
        });
    }
}
