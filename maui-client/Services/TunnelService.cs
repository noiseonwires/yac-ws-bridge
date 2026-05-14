using System.Net;
using System.Net.WebSockets;

namespace BridgeToFreedom.Services;

/// <summary>
/// Manages the upstream WebSocket to the YC API Gateway and exposes a packet
/// I/O surface. Knows nothing about TUN devices: a platform-specific service
/// (Android <c>BtfVpnService</c>) feeds it IP packets via <see cref="SendPacketAsync"/>
/// and consumes inbound packets via <see cref="OnInboundPacket"/>.
/// </summary>
public sealed class TunnelService : IDisposable
{
    public event Action<string>? OnLog;
    public event Action? OnStopped;

    /// <summary>Raised on the upstream read thread when a peer-originated IP packet arrives.</summary>
    public event Action<byte[]>? OnInboundPacket;

    private CancellationTokenSource? _cts;
    private volatile bool _stopping;
    private ClientWebSocket? _upstream;
    private readonly SemaphoreSlim _writeLock = new(1, 1);

    private volatile bool _upstreamReady;
    private string _ownConnId = "";
    private string _peerConnId = "";
    private string _staleConnId = "";
    private string _iamToken = "";

    // tx instrumentation: counted from the moment a packet enters SendToPeerAsync.
    // Reported once per StatsIntervalMs by StatsLoopAsync.
    private long _txOffered;
    private long _txDroppedNotReady;
    private long _txDroppedNoPeer;
    private long _txDroppedNoToken;
    private long _txSentRelay;
    private long _txSentApi;
    private long _txSendErr;
    private long _rxPackets;
    private long _rxBatchPackets;
    private long _syncSentTicks; // last SYNC send tick (debounce)
    private const int SyncDebounceMs = 500;

    // --- Config ---
    public string BridgeUrl { get; set; } = "";
    public string AuthToken { get; set; } = "";
    public bool Relay { get; set; }
    public int PingIntervalMs { get; set; } = 30000;
    public int StatsIntervalMs { get; set; } = 5000;

    public bool IsRunning => _cts != null && !_cts.IsCancellationRequested;
    public bool IsReady => _upstreamReady && (Relay || !string.IsNullOrEmpty(_peerConnId));

    public async Task StartAsync()
    {
        if (_cts != null) { Log("StartAsync: already running"); return; }
        _cts = new CancellationTokenSource();
        _upstreamReady = false;
        _stopping = false;
        var ct = _cts.Token;

        Log("Starting tunnel service...");
        try { await UpstreamLoopAsync(ct); }
        catch (OperationCanceledException) { }
        catch (Exception ex) { Log($"Service error: {ex.Message}"); }

        _upstreamReady = false;
        Log("Tunnel service stopped.");
        OnStopped?.Invoke();
    }

    public void Stop()
    {
        _stopping = true;
        Log("Stop requested.");
        var cts = _cts;
        _cts = null;
        try { cts?.Cancel(); } catch { }
    }

    /// <summary>
    /// Sends one IP packet to the peer. Drops silently when the peer is not
    /// known (TCP retransmission will recover).
    /// </summary>
    public Task SendPacketAsync(byte[] packet, CancellationToken ct = default)
        => SendToPeerAsync(Protocol.Encode(Protocol.MsgPacket, packet), ct);

    /// <summary>
    /// Sends a batch of IP packets in a single PACKET_BATCH frame.
    /// </summary>
    public Task SendPacketBatchAsync(IReadOnlyList<byte[]> packets, CancellationToken ct = default)
    {
        if (packets.Count == 0) return Task.CompletedTask;
        if (packets.Count == 1) return SendPacketAsync(packets[0], ct);
        return SendToPeerAsync(Protocol.Encode(Protocol.MsgPacketBatch, Protocol.EncodePacketBatch(packets)), ct);
    }

    // --- Upstream ---

    private async Task UpstreamLoopAsync(CancellationToken ct)
    {
        int delay = 1000;
        while (!ct.IsCancellationRequested)
        {
            try
            {
                Log($"Connecting to {BridgeUrl}...");
                var bridgeUri = new Uri(BridgeUrl);
                IPAddress[] addresses = Array.Empty<IPAddress>();
                try { addresses = await Dns.GetHostAddressesAsync(bridgeUri.Host); }
                catch (Exception ex) { Log($"DNS resolve failed for {bridgeUri.Host}: {ex.Message}"); }

                var ws = new ClientWebSocket();
                ws.Options.KeepAliveInterval = TimeSpan.FromSeconds(30);
                _upstream = ws;

                using var connCts = CancellationTokenSource.CreateLinkedTokenSource(ct);
                connCts.CancelAfter(TimeSpan.FromSeconds(10));
                await ws.ConnectAsync(bridgeUri, connCts.Token);

                Log("WebSocket connected, sending HELLO...");
                await WsSendUpstream(Protocol.Encode(Protocol.MsgHello, Protocol.EncodeHello(0x01, AuthToken)), ct);

                var resp = await WsReceiveWithTimeout(TimeSpan.FromSeconds(10), ct);
                if (resp == null) { Log("No HELLO response, retrying..."); continue; }

                var (type, payload) = Protocol.Decode(resp);
                if (type == Protocol.MsgHelloErr)
                {
                    Log($"Auth rejected: {System.Text.Encoding.UTF8.GetString(payload)}");
                    await Task.Delay(5000, ct);
                    continue;
                }
                if (type != Protocol.MsgHelloOK)
                {
                    Log($"Unexpected response: {Protocol.MsgName(type)}, retrying...");
                    continue;
                }

                var (ownId, peerId, iamToken) = Protocol.DecodeHelloOK(payload);
                _ownConnId = ownId;
                _peerConnId = peerId;
                _iamToken = iamToken;
                _staleConnId = "";
                delay = 1000;

                Log($"Authenticated. ownId={Shorten(ownId)} peerId={Shorten(peerId)}");
                if (string.IsNullOrEmpty(peerId) && !Relay)
                {
                    Log("Peer unknown after HELLO_OK; sending SYNC");
                    try { await WsSendUpstream(Protocol.Encode(Protocol.MsgSync), ct); } catch { }
                    _syncSentTicks = Environment.TickCount;
                }
                LogDns("Gateway", bridgeUri.Host, addresses);
                if (!Relay)
                {
                    try
                    {
                        var cloudApiHost = "apigateway-connections.api.cloud.yandex.net";
                        var cloudIps = await Dns.GetHostAddressesAsync(cloudApiHost);
                        LogDns("Cloud API", cloudApiHost, cloudIps);
                    }
                    catch (Exception ex) { Log($"Cloud API DNS resolve failed: {ex.Message}"); }
                }
                else
                {
                    Log("Relay mode: Cloud API will NOT be used");
                }
                _upstreamReady = true;

                using var pingCts = CancellationTokenSource.CreateLinkedTokenSource(ct);
                var pingTask = PingLoopAsync(pingCts.Token);
                var statsTask = StatsLoopAsync(pingCts.Token);
                await ReadLoopAsync(ct);
                pingCts.Cancel();
                try { await pingTask; } catch { }
                try { await statsTask; } catch { }
                _upstreamReady = false;
                Log("Upstream disconnected.");
            }
            catch (OperationCanceledException) when (ct.IsCancellationRequested) { return; }
            catch (Exception ex) { Log($"Upstream error: {ex.Message}"); }

            _upstreamReady = false;
            CloseUpstream();
            _ownConnId = "";
            _peerConnId = "";
            _iamToken = "";

            if (ct.IsCancellationRequested) return;
            Log($"Reconnecting in {delay}ms...");
            try { await Task.Delay(delay, ct); } catch { return; }
            delay = Math.Min(delay * 2, 30000);
        }
    }

    private async Task ReadLoopAsync(CancellationToken ct)
    {
        // 256 KiB upper bound: a single PACKET_BATCH may carry ~80 MTUs.
        var buffer = new byte[256 * 1024];
        while (!ct.IsCancellationRequested && _upstream?.State == WebSocketState.Open)
        {
            try
            {
                int totalRead = 0;
                WebSocketReceiveResult result;
                do
                {
                    if (totalRead >= buffer.Length)
                    {
                        Log($"Message too large, dropping (>{buffer.Length} bytes)");
                        return;
                    }
                    result = await _upstream!.ReceiveAsync(
                        new ArraySegment<byte>(buffer, totalRead, buffer.Length - totalRead), ct);
                    if (result.MessageType == WebSocketMessageType.Close) return;
                    totalRead += result.Count;
                } while (!result.EndOfMessage);

                if (result.MessageType != WebSocketMessageType.Binary) continue;

                var data = new byte[totalRead];
                Buffer.BlockCopy(buffer, 0, data, 0, totalRead);
                HandleFrame(data);
            }
            catch (OperationCanceledException) { return; }
            catch (WebSocketException) { return; }
            catch (Exception ex) { Log($"Read error: {ex.Message}"); return; }
        }
    }

    private void HandleFrame(byte[] data)
    {
        try
        {
            var (type, payload) = Protocol.Decode(data);
            switch (type)
            {
                case Protocol.MsgPeerConn:
                    var (peerId, iamToken) = Protocol.DecodePeerConn(payload);
                    if (peerId == _staleConnId && peerId != "")
                    {
                        Log($"PEER_CONN with stale ID {Shorten(peerId)}, ignoring");
                        return;
                    }
                    _staleConnId = "";
                    var prevPeer = _peerConnId;
                    _peerConnId = peerId;
                    if (iamToken != "") _iamToken = iamToken;
                    if (prevPeer != peerId)
                        Log($"Peer connected: {Shorten(peerId)}");
                    break;

                case Protocol.MsgPeerGone:
                    Log("Peer gone");
                    _peerConnId = "";
                    break;

                case Protocol.MsgPong:
                    var token = Protocol.DecodePong(payload);
                    if (token != "") _iamToken = token;
                    break;

                case Protocol.MsgPacket:
                    Interlocked.Increment(ref _rxPackets);
                    OnInboundPacket?.Invoke(payload);
                    break;

                case Protocol.MsgPacketBatch:
                    Protocol.DecodePacketBatch(payload, seg =>
                    {
                        Interlocked.Increment(ref _rxBatchPackets);
                        var copy = new byte[seg.Count];
                        Buffer.BlockCopy(seg.Array!, seg.Offset, copy, 0, seg.Count);
                        OnInboundPacket?.Invoke(copy);
                    });
                    break;

                default:
                    Log($"Unknown frame type {Protocol.MsgName(type)}");
                    break;
            }
        }
        catch (Exception ex) { Log($"Frame error: {ex.Message}"); }
    }

    // --- Send to peer ---

    private async Task SendToPeerAsync(byte[] frame, CancellationToken ct)
    {
        Interlocked.Increment(ref _txOffered);
        if (Relay)
        {
            if (!_upstreamReady) { Interlocked.Increment(ref _txDroppedNotReady); return; }
            try { await WsSendUpstream(frame, ct); Interlocked.Increment(ref _txSentRelay); }
            catch (Exception ex) { Interlocked.Increment(ref _txSendErr); Log($"Relay send failed: {ex.Message}"); }
            return;
        }

        var peer = _peerConnId;
        var token = _iamToken;
        if (string.IsNullOrEmpty(peer))
        {
            Interlocked.Increment(ref _txDroppedNoPeer);
            // Debounced SYNC to ask cloud to re-announce peer connId.
            var now = Environment.TickCount;
            if (now - _syncSentTicks > SyncDebounceMs)
            {
                _syncSentTicks = now;
                try { await WsSendUpstream(Protocol.Encode(Protocol.MsgSync), ct); } catch { }
            }
            return;
        }
        if (string.IsNullOrEmpty(token)) { Interlocked.Increment(ref _txDroppedNoToken); return; }

        try { await WsSendApi(peer, frame, token, ct); Interlocked.Increment(ref _txSentApi); }
        catch (Exception ex)
        {
            Interlocked.Increment(ref _txSendErr);
            _staleConnId = _peerConnId;
            _peerConnId = "";
            Log($"wsSend failed: {ex.GetType().Name}: {ex.Message} (peer={peer}), marked stale, sending SYNC");
            try { await WsSendUpstream(Protocol.Encode(Protocol.MsgSync), ct); } catch { }
        }
    }

    private static readonly HttpClient _http = new() { Timeout = TimeSpan.FromSeconds(10) };

    private async Task WsSendApi(string connId, byte[] data, string iamToken, CancellationToken ct)
    {
        var b64 = Convert.ToBase64String(data);
        var json = $"{{\"data\":\"{b64}\",\"type\":\"BINARY\"}}";
        var url = $"https://apigateway-connections.api.cloud.yandex.net/apigateways/websocket/v1/connections/{Uri.EscapeDataString(connId)}:send";
        using var req = new HttpRequestMessage(HttpMethod.Post, url);
        req.Content = new StringContent(json, System.Text.Encoding.UTF8, "application/json");
        req.Headers.Authorization = new System.Net.Http.Headers.AuthenticationHeaderValue("Bearer", iamToken);
        var resp = await _http.SendAsync(req, ct);
        if (!resp.IsSuccessStatusCode)
        {
            var body = await resp.Content.ReadAsStringAsync(ct);
            throw new Exception($"wsSend {(int)resp.StatusCode}: {body}");
        }
    }

    private async Task WsSendUpstream(byte[] data, CancellationToken ct)
    {
        var ws = _upstream;
        if (ws == null || ws.State != WebSocketState.Open)
            throw new InvalidOperationException("upstream not connected");
        await _writeLock.WaitAsync(ct);
        try { await ws.SendAsync(new ArraySegment<byte>(data), WebSocketMessageType.Binary, true, ct); }
        finally { _writeLock.Release(); }
    }

    private async Task<byte[]?> WsReceiveWithTimeout(TimeSpan timeout, CancellationToken ct)
    {
        var ws = _upstream;
        if (ws == null) return null;
        using var toCts = CancellationTokenSource.CreateLinkedTokenSource(ct);
        toCts.CancelAfter(timeout);
        var buffer = new byte[64 * 1024];
        try
        {
            int totalRead = 0;
            WebSocketReceiveResult result;
            do
            {
                result = await ws.ReceiveAsync(
                    new ArraySegment<byte>(buffer, totalRead, buffer.Length - totalRead), toCts.Token);
                if (result.MessageType == WebSocketMessageType.Close) return null;
                totalRead += result.Count;
            } while (!result.EndOfMessage);
            var data = new byte[totalRead];
            Buffer.BlockCopy(buffer, 0, data, 0, totalRead);
            return data;
        }
        catch (OperationCanceledException) when (!ct.IsCancellationRequested) { return null; }
    }

    private async Task PingLoopAsync(CancellationToken ct)
    {
        try
        {
            while (!ct.IsCancellationRequested && _upstream?.State == WebSocketState.Open)
            {
                await Task.Delay(PingIntervalMs, ct);
                await WsSendUpstream(Protocol.Encode(Protocol.MsgPing), ct);
            }
        }
        catch (OperationCanceledException) { }
        catch (Exception ex) { Log($"Ping error: {ex.Message}"); }
    }

    private async Task StatsLoopAsync(CancellationToken ct)
    {
        Log($"Stats loop starting (interval={StatsIntervalMs}ms)");
        try
        {
            while (!ct.IsCancellationRequested)
            {
                await Task.Delay(StatsIntervalMs, ct);
                var off  = Interlocked.Read(ref _txOffered);
                var dnr  = Interlocked.Read(ref _txDroppedNotReady);
                var dnp  = Interlocked.Read(ref _txDroppedNoPeer);
                var dnt  = Interlocked.Read(ref _txDroppedNoToken);
                var sR   = Interlocked.Read(ref _txSentRelay);
                var sA   = Interlocked.Read(ref _txSentApi);
                var serr = Interlocked.Read(ref _txSendErr);
                var rxP  = Interlocked.Read(ref _rxPackets);
                var rxB  = Interlocked.Read(ref _rxBatchPackets);
                Log($"stats tx[off={off} sentApi={sA} sentRelay={sR} err={serr} drop(notReady={dnr},noPeer={dnp},noToken={dnt})] rx[pkt={rxP} batchPkt={rxB}] state[ready={_upstreamReady} peer={Shorten(_peerConnId)} tokenLen={_iamToken.Length}]");
            }
        }
        catch (OperationCanceledException) { }
        catch (Exception ex) { Log($"Stats error: {ex.Message}"); }
    }

    private void CloseUpstream()
    {
        var ws = _upstream;
        _upstream = null;
        if (ws != null)
        {
            try { ws.Abort(); } catch { }
            try { ws.Dispose(); } catch { }
        }
    }

    private void Log(string msg)
    {
        if (_stopping) return;
        var line = $"[{DateTime.Now:HH:mm:ss}] {msg}";
        OnLog?.Invoke(line);
        System.Diagnostics.Debug.WriteLine(line);
    }

    /// <summary>External callers (platform VPN service) can route log lines through
    /// the same sink so they appear in the in-app log and logcat together.</summary>
    public void LogExternal(string msg) => Log(msg);

    private void LogDns(string label, string host, IPAddress[] addresses)
    {
        var v4 = addresses.Where(a => a.AddressFamily == System.Net.Sockets.AddressFamily.InterNetwork).ToArray();
        var v6 = addresses.Where(a => a.AddressFamily == System.Net.Sockets.AddressFamily.InterNetworkV6).ToArray();
        var parts = new List<string>();
        if (v4.Length > 0) parts.Add($"A: {string.Join(", ", v4.Select(a => a.ToString()))}");
        if (v6.Length > 0) parts.Add($"AAAA: {string.Join(", ", v6.Select(a => a.ToString()))}");
        Log($"{label}: {host} -> [{string.Join(" | ", parts)}]");
    }

    private static string Shorten(string id) =>
        id.Length > 12 ? id[..8] + "..." + id[^4..] : id;

    public void Dispose()
    {
        Stop();
        CloseUpstream();
    }
}
