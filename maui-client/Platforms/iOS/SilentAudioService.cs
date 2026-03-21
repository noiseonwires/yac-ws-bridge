#if IOS_BACKGROUND_AUDIO
using AVFoundation;
using Foundation;

namespace BridgeToFreedom.Platforms.iOS;

/// <summary>
/// Plays a silent audio loop to keep the app alive in the background on iOS.
/// Only compiled when IOS_BACKGROUND_AUDIO is defined.
/// </summary>
public static class SilentAudioService
{
    private static AVAudioPlayer? _player;
    private static bool _running;

    public static void Start()
    {
        if (_running) return;

        try
        {
            // Configure audio session for background playback
            var session = AVAudioSession.SharedInstance();
            session.SetCategory(AVAudioSessionCategory.Playback, AVAudioSessionCategoryOptions.MixWithOthers);
            session.SetActive(true);

            // Load the silent audio file from app bundle
            var url = NSBundle.MainBundle.GetUrlForResource("silence", "wav");
            if (url == null)
            {
                System.Diagnostics.Debug.WriteLine("[iOS] silence.wav not found in bundle");
                return;
            }

            _player = AVAudioPlayer.FromUrl(url);
            if (_player == null)
            {
                System.Diagnostics.Debug.WriteLine("[iOS] Failed to create audio player");
                return;
            }

            _player.NumberOfLoops = -1; // Loop forever
            _player.Volume = 0.0f;      // Completely silent
            _player.PrepareToPlay();
            _player.Play();
            _running = true;

            System.Diagnostics.Debug.WriteLine("[iOS] Silent audio started for background keep-alive");
        }
        catch (Exception ex)
        {
            System.Diagnostics.Debug.WriteLine($"[iOS] Silent audio error: {ex.Message}");
        }
    }

    public static void Stop()
    {
        if (!_running) return;

        try
        {
            _player?.Stop();
            _player?.Dispose();
            _player = null;

            AVAudioSession.SharedInstance().SetActive(false);
        }
        catch { }

        _running = false;
        System.Diagnostics.Debug.WriteLine("[iOS] Silent audio stopped");
    }
}
#endif
