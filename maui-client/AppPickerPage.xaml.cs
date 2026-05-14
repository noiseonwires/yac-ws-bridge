using System.Collections.ObjectModel;
using System.ComponentModel;
using System.Runtime.CompilerServices;
using Android.Content.PM;
using Android.Graphics;
using Android.Graphics.Drawables;

namespace BridgeToFreedom;

public partial class AppPickerPage : ContentPage
{
    private readonly TaskCompletionSource<HashSet<string>?> _result =
        new(TaskCreationOptions.RunContinuationsAsynchronously);

    private List<AppEntry> _allApps = new();
    private readonly ObservableCollection<AppEntry> _filtered = new();
    private string _filter = "";

    /// <summary>
    /// Resolves with the chosen package set on Save, or null on Cancel / back-navigation.
    /// </summary>
    public Task<HashSet<string>?> Result => _result.Task;

    public AppPickerPage(IEnumerable<string> initiallySelected)
    {
        InitializeComponent();
        AppsList.ItemsSource = _filtered;

        var initial = new HashSet<string>(initiallySelected, StringComparer.Ordinal);
        _ = LoadAppsAsync(initial);
    }

    private async Task LoadAppsAsync(HashSet<string> initiallySelected)
    {
        try
        {
            var loaded = await Task.Run(() => EnumerateApps(initiallySelected));
            _allApps = loaded;
            ApplyFilter();
            UpdateCount();
        }
        catch (Exception ex)
        {
            await DisplayAlertAsync("Error", $"Failed to enumerate apps: {ex.Message}", "OK");
            _result.TrySetResult(null);
            await Navigation.PopModalAsync();
        }
    }

    private static List<AppEntry> EnumerateApps(HashSet<string> selected)
    {
        var ctx = global::Android.App.Application.Context;
        var pm  = ctx.PackageManager!;
        var ownPackage = ctx.PackageName;

        // GetInstalledApplications gives every application record. We then keep
        // only ones that have a launcher entry (i.e. the user-facing apps),
        // and drop our own package since it can never be routed through the VPN.
        var apps = pm.GetInstalledApplications(PackageInfoFlags.MatchAll);
        var result = new List<AppEntry>(apps.Count);
        foreach (var info in apps)
        {
            var pkg = info.PackageName;
            if (string.IsNullOrEmpty(pkg) || pkg == ownPackage) continue;
            if (pm.GetLaunchIntentForPackage(pkg) == null) continue;

            string label;
            try { label = pm.GetApplicationLabel(info)?.ToString() ?? pkg; }
            catch { label = pkg; }

            ImageSource? icon = null;
            try
            {
                var drawable = pm.GetApplicationIcon(info);
                icon = DrawableToImageSource(drawable);
            }
            catch { /* ignore: missing icon */ }

            result.Add(new AppEntry
            {
                Package    = pkg,
                Label      = label,
                IconSource = icon,
                IsChecked  = selected.Contains(pkg),
            });
        }

        // Sort with selected apps first, then alphabetically.
        result.Sort((a, b) =>
        {
            if (a.IsChecked != b.IsChecked) return b.IsChecked.CompareTo(a.IsChecked);
            return string.Compare(a.Label, b.Label, StringComparison.CurrentCultureIgnoreCase);
        });
        return result;
    }

    private static ImageSource? DrawableToImageSource(Drawable? drawable)
    {
        if (drawable == null) return null;
        Bitmap? bmp = null;
        if (drawable is BitmapDrawable bd && bd.Bitmap != null)
        {
            bmp = bd.Bitmap;
        }
        else
        {
            int w = Math.Max(1, drawable.IntrinsicWidth);
            int h = Math.Max(1, drawable.IntrinsicHeight);
            bmp = Bitmap.CreateBitmap(w, h, Bitmap.Config.Argb8888!);
            using var canvas = new Canvas(bmp);
            drawable.SetBounds(0, 0, w, h);
            drawable.Draw(canvas);
        }
        if (bmp == null) return null;

        using var stream = new MemoryStream();
        bmp.Compress(Bitmap.CompressFormat.Png!, 100, stream);
        var bytes = stream.ToArray();
        // ImageSource.FromStream copies the stream lazily on each render, so
        // we capture the bytes into a closure to keep them alive.
        return ImageSource.FromStream(() => new MemoryStream(bytes));
    }

    private void ApplyFilter()
    {
        _filtered.Clear();
        if (string.IsNullOrWhiteSpace(_filter))
        {
            foreach (var a in _allApps) _filtered.Add(a);
            return;
        }
        var f = _filter.Trim();
        foreach (var a in _allApps)
        {
            if (a.Label.Contains(f, StringComparison.CurrentCultureIgnoreCase) ||
                a.Package.Contains(f, StringComparison.OrdinalIgnoreCase))
            {
                _filtered.Add(a);
            }
        }
    }

    private void UpdateCount()
    {
        var n = _allApps.Count(a => a.IsChecked);
        CountLabel.Text = n == 0
            ? "No apps selected — VPN will route ALL apps"
            : $"{n} selected";
    }

    private void OnFilterChanged(object? sender, TextChangedEventArgs e)
    {
        _filter = e.NewTextValue ?? "";
        ApplyFilter();
    }

    private void OnRowTapped(object? sender, TappedEventArgs e)
    {
        if (sender is BindableObject bo && bo.BindingContext is AppEntry entry)
        {
            entry.IsChecked = !entry.IsChecked;
            UpdateCount();
        }
    }

    private void OnRowCheckedChanged(object? sender, CheckedChangedEventArgs e) => UpdateCount();

    private void OnSelectAllClicked(object? sender, EventArgs e)
    {
        // Select all currently visible (filtered) entries.
        foreach (var a in _filtered) a.IsChecked = true;
        UpdateCount();
    }

    private void OnClearClicked(object? sender, EventArgs e)
    {
        // Clear ALL selections (not just filtered) so the user gets a clean slate.
        foreach (var a in _allApps) a.IsChecked = false;
        UpdateCount();
    }

    private async void OnDoneClicked(object? sender, EventArgs e)
    {
        var selected = new HashSet<string>(
            _allApps.Where(a => a.IsChecked).Select(a => a.Package),
            StringComparer.Ordinal);
        _result.TrySetResult(selected);
        await Navigation.PopModalAsync();
    }

    private async void OnCancelClicked(object? sender, EventArgs e)
    {
        _result.TrySetResult(null);
        await Navigation.PopModalAsync();
    }

    protected override bool OnBackButtonPressed()
    {
        // Treat hardware back as cancel.
        _result.TrySetResult(null);
        return base.OnBackButtonPressed();
    }

    public sealed class AppEntry : INotifyPropertyChanged
    {
        public string Package { get; init; } = "";
        public string Label   { get; init; } = "";
        public ImageSource? IconSource { get; init; }

        private bool _isChecked;
        public bool IsChecked
        {
            get => _isChecked;
            set
            {
                if (_isChecked == value) return;
                _isChecked = value;
                OnPropertyChanged();
            }
        }

        public event PropertyChangedEventHandler? PropertyChanged;
        private void OnPropertyChanged([CallerMemberName] string? n = null)
            => PropertyChanged?.Invoke(this, new PropertyChangedEventArgs(n));
    }
}
