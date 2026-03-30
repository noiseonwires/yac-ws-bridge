using System.Collections.Concurrent;
using System.Net;
using System.Net.Sockets;
using System.Net.WebSockets;

namespace BridgeToFreedom.Services;

/// <summary>
/// Core tunnel service matching the Go helper logic.
/// Manages upstream WS, TCP listener, stream multiplexing, and wsSend via YC gRPC API (or relay).
/// </summary>
public sealed class TunnelService : IDisposable
{
    public event Action<string>? OnLog;
    public event Action? OnStopped;

    private CancellationTokenSource? _cts;
    private volatile bool _stopping;
    private ClientWebSocket? _upstream;
    private TcpListener? _listener;
    private readonly SemaphoreSlim _writeLock = new(1, 1);

    // State
    private volatile bool _upstreamReady;
    private string _ownConnId = "";
    private string _peerConnId = "";
    private string _staleConnId = "";
    private string _iamToken = "";
    private uint _nextStreamId;

    private readonly ConcurrentDictionary<uint, StreamState> _streams = new();
    private readonly ConcurrentDictionary<uint, TaskCompletionSource<byte[]>> _pendingOpens = new();
    private readonly ConcurrentDictionary<uint, uint> _streamSeqs = new();

    // Config
    public string BridgeUrl { get; set; } = "";
    public string AuthToken { get; set; } = "";
    public string ListenAddress { get; set; } = "0.0.0.0";
    public int ListenPort { get; set; } = 5080;
    public bool Relay { get; set; }
    public int PingIntervalMs { get; set; } = 30000;
    public bool WriteCoalescing { get; set; }
    public int WriteCoalescingDelayMs { get; set; } = 10;

    public bool IsRunning => _cts != null && !_cts.IsCancellationRequested;

    private sealed class StreamState : IDisposable
    {
        public uint Id;
        public TcpClient Client;
        public NetworkStream NetStream;
        public CancellationTokenSource Cts;
        public volatile bool Closed;

        public StreamState(uint id, TcpClient client)
        {
            Id = id;
            Client = client;
            NetStream = client.GetStream();
            Cts = new CancellationTokenSource();
        }

        public void Dispose()
        {
            if (Closed) return;
            Closed = true;
            Cts.Cancel();
            try { NetStream.Dispose(); } catch { }
            try { Client.Dispose(); } catch { }
            Cts.Dispose();
        }
    }

    public async Task StartAsync()
    {
        if (_cts != null)
        {
            Log("StartAsync called but already running, ignoring.");
            return;
        }
        _cts = new CancellationTokenSource();
        _nextStreamId = 1;
        _upstreamReady = false;
        var ct = _cts.Token;

        Log("Starting tunnel service...");
        _stopping = false;

        try
        {
            // Run upstream loop — it starts the listener after first successful connect
            await UpstreamLoopAsync(ct);
        }
        catch (OperationCanceledException) { }
        catch (Exception ex) { Log($"Service error: {ex.Message}"); }

        _upstreamReady = false;
        CloseAllStreams();
        StopListener();
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

    // --- Upstream WebSocket loop ---

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
                try
                {
                    addresses = await Dns.GetHostAddressesAsync(bridgeUri.Host);
                }
                catch (Exception ex)
                {
                    Log($"DNS resolve failed for {bridgeUri.Host}: {ex.Message}");
                }
                var ws = new ClientWebSocket();
                ws.Options.KeepAliveInterval = TimeSpan.FromSeconds(30);
                _upstream = ws;

                using var connCts = CancellationTokenSource.CreateLinkedTokenSource(ct);
                connCts.CancelAfter(TimeSpan.FromSeconds(10));
                await ws.ConnectAsync(bridgeUri, connCts.Token);

                Log("WebSocket connected, sending HELLO...");
                var hello = Protocol.Encode(Protocol.MsgHello, 0, Protocol.EncodeHello(0x01, AuthToken));
                await WsSendUpstream(hello, ct);

                Log("HELLO sent, waiting for HELLO_OK...");
                var resp = await WsReceiveWithTimeout(TimeSpan.FromSeconds(10), ct);
                if (resp == null) { Log("No HELLO response, retrying..."); continue; }

                var (type, _, _, payload) = Protocol.Decode(resp);
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
                LogDns("Gateway", bridgeUri.Host, addresses);
                if (!Relay)
                {
                    try
                    {
                        var cloudApiHost = "apigateway-connections.api.cloud.yandex.net";
                        var cloudIps = await Dns.GetHostAddressesAsync(cloudApiHost);
                        LogDns("Cloud API", cloudApiHost, cloudIps);
                    }
                    catch (Exception ex)
                    {
                        Log($"Cloud API DNS resolve failed: {ex.Message}");
                    }
                }
                else
                {
                    Log("Relay mode: Cloud API will NOT be used, all data goes through upstream WS");
                }
                _upstreamReady = true;

                // Start listener if not already running
                EnsureListenerRunning(ct);

                // Start ping loop and read loop
                using var pingCts = CancellationTokenSource.CreateLinkedTokenSource(ct);
                var pingTask = PingLoopAsync(pingCts.Token);
                await ReadLoopAsync(ct);
                pingCts.Cancel();
                try { await pingTask; } catch { }
                _upstreamReady = false;
                Log("Upstream disconnected.");
            }
            catch (OperationCanceledException) when (ct.IsCancellationRequested) { return; }
            catch (Exception ex) { Log($"Upstream error: {ex.Message}"); }

            // Clean up
            _upstreamReady = false;
            CloseUpstream();
            _ownConnId = "";
            _peerConnId = "";
            _iamToken = "";
            CloseAllStreams();

            if (ct.IsCancellationRequested) return;
            Log($"Reconnecting in {delay}ms...");
            try { await Task.Delay(delay, ct); } catch { return; }
            delay = Math.Min(delay * 2, 30000);
        }
    }

    private async Task ReadLoopAsync(CancellationToken ct)
    {
        var buffer = new byte[128 * 1024];
        while (!ct.IsCancellationRequested && _upstream?.State == WebSocketState.Open)
        {
            try
            {
                // Read full message (may arrive in multiple frames)
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
            var (type, streamId, _, payload) = Protocol.Decode(data);

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
                    // Cancel pending opens from previous peer session
                    CancelPendingOpens("new peer connected");
                    _peerConnId = peerId;
                    if (iamToken != "") _iamToken = iamToken;
                    Log($"Peer connected: {Shorten(peerId)}");
                    break;

                case Protocol.MsgPeerGone:
                    Log($"Peer gone, closing {_streams.Count} streams");
                    _peerConnId = "";
                    CancelPendingOpens("peer gone");
                    CloseAllStreams();
                    break;

                case Protocol.MsgPong:
                    var token = Protocol.DecodePong(payload);
                    if (token != "") _iamToken = token;
                    break;

                case Protocol.MsgOpenOK:
                case Protocol.MsgOpenFail:
                    if (_pendingOpens.TryRemove(streamId, out var tcs))
                        tcs.TrySetResult(data);
                    else
                        Log($"{Protocol.MsgName(type)} for unknown stream={streamId}");
                    break;

                case Protocol.MsgData:
                    if (_streams.TryGetValue(streamId, out var s) && !s.Closed)
                    {
                        try { s.NetStream.Write(payload, 0, payload.Length); }
                        catch { CloseStream(s); }
                    }
                    break;

                case Protocol.MsgFin:
                    Log($"FIN stream={streamId}");
                    if (_streams.TryGetValue(streamId, out var fs))
                        CloseStream(fs);
                    break;

                case Protocol.MsgRst:
                    Log($"RST stream={streamId}");
                    if (_streams.TryGetValue(streamId, out var rs))
                        CloseStream(rs);
                    break;
            }
        }
        catch (Exception ex) { Log($"Frame error: {ex.Message}"); }
    }

    // --- TCP Listener ---

    private Task? _listenerTask;

    private void EnsureListenerRunning(CancellationToken ct)
    {
        if (_listenerTask != null && !_listenerTask.IsCompleted) return;
        StopListener();
        Log("Starting TCP listener...");
        _listenerTask = Task.Run(() => ListenerLoopAsync(ct), ct);
    }

    private void StopListener()
    {
        try { _listener?.Stop(); } catch { }
        _listener = null;
    }

    private async Task ListenerLoopAsync(CancellationToken ct)
    {
        var ip = IPAddress.Parse(ListenAddress);
        _listener = new TcpListener(ip, ListenPort);
        _listener.Start();
        Log($"Listening on {ListenAddress}:{ListenPort}");

        try
        {
            while (!ct.IsCancellationRequested)
            {
                TcpClient client;
                try
                {
                    client = await _listener.AcceptTcpClientAsync(ct);
                }
                catch (OperationCanceledException) { break; }
                catch (SocketException) when (ct.IsCancellationRequested) { break; }
                catch (ObjectDisposedException) { break; }
                catch (Exception ex)
                {
                    if (ct.IsCancellationRequested) break;
                    Log($"Accept error: {ex.Message}");
                    await Task.Delay(500, ct);
                    continue;
                }
                client.NoDelay = true;
                _ = Task.Run(() => HandleClientAsync(client, ct), ct);
            }
        }
        catch (OperationCanceledException) { }
        catch (SocketException) when (ct.IsCancellationRequested) { }
        catch (Exception ex) { Log($"Listener error: {ex.Message}"); }
        finally
        {
            try { _listener.Stop(); } catch { }
            Log("Listener stopped.");
        }
    }

    private async Task HandleClientAsync(TcpClient client, CancellationToken ct)
    {
        var sid = Interlocked.Increment(ref _nextStreamId) - 1;
        var remote = client.Client.RemoteEndPoint?.ToString() ?? "?";
        Log($"New connection remote={remote} stream={sid}");

        var state = new StreamState(sid, client);
        var tcs = new TaskCompletionSource<byte[]>();
        _pendingOpens[sid] = tcs;

        try
        {
            // Wait until upstream is connected and peer is known (up to 10s)
            if (!await WaitForReadyAsync(ct))
            {
                Log($"OPEN failed stream={sid}: not ready (no upstream or peer)");
                client.Dispose();
                return;
            }

            // Send OPEN
            var err = await SendToPeerAsync(Protocol.Encode(Protocol.MsgOpen, sid, NextSeq(sid)));
            if (err != null) { Log($"OPEN failed stream={sid}: {err}"); client.Dispose(); return; }

            // Wait for OPEN_OK/OPEN_FAIL
            using var openCts = CancellationTokenSource.CreateLinkedTokenSource(ct);
            openCts.CancelAfter(TimeSpan.FromSeconds(10));
            byte[] resp;
            try { resp = await tcs.Task.WaitAsync(openCts.Token); }
            catch (OperationCanceledException) { Log($"OPEN timeout stream={sid}"); client.Dispose(); return; }
            finally { _pendingOpens.TryRemove(sid, out _); }

            var (type, _, _, payload) = Protocol.Decode(resp);
            if (type == Protocol.MsgOpenFail)
            {
                Log($"OPEN rejected stream={sid}");
                client.Dispose();
                return;
            }

            // Stream open — register and start forwarding
            _streams[sid] = state;
            Log($"Stream opened stream={sid} remote={remote}");

            await TcpReadLoopAsync(state, ct);
        }
        catch (Exception ex) { Log($"Stream {sid} error: {ex.Message}"); }
        finally
        {
            if (!state.Closed)
            {
                // Send FIN
                _ = SendToPeerAsync(Protocol.Encode(Protocol.MsgFin, sid, NextSeq(sid)));
            }
            CloseStream(state);
        }
    }

    private async Task TcpReadLoopAsync(StreamState state, CancellationToken ct)
    {
        var buf = new byte[32 * 1024];
        byte[]? coalesceBuf = null;
        var coalesce = WriteCoalescing && WriteCoalescingDelayMs > 0;

        async Task FlushCoalesce()
        {
            if (coalesceBuf == null || coalesceBuf.Length == 0) return;
            var data = coalesceBuf;
            coalesceBuf = null;
            var err = await SendToPeerAsync(Protocol.Encode(Protocol.MsgData, state.Id, NextSeq(state.Id), data));
            if (err != null) Log($"Send DATA failed stream={state.Id}: {err}");
        }

        try
        {
            using var linked = CancellationTokenSource.CreateLinkedTokenSource(ct, state.Cts.Token);
            while (!linked.IsCancellationRequested)
            {
                int n;
                if (coalesce && coalesceBuf != null && coalesceBuf.Length > 0)
                {
                    // We have buffered data. Check if more is already waiting.
                    if (state.NetStream.DataAvailable)
                    {
                        // Data in kernel buffer — read immediately without blocking
                        n = state.NetStream.Read(buf, 0, buf.Length);
                    }
                    else
                    {
                        // Nothing waiting — give it coalescingMs for more data to arrive
                        await Task.Delay(WriteCoalescingDelayMs, linked.Token);
                        if (state.NetStream.DataAvailable)
                        {
                            n = state.NetStream.Read(buf, 0, buf.Length);
                        }
                        else
                        {
                            // Still nothing — flush what we have and block for next read
                            await FlushCoalesce();
                            continue;
                        }
                    }
                }
                else
                {
                    // No buffered data (or coalescing off) — block until data arrives
                    n = await state.NetStream.ReadAsync(buf, 0, buf.Length, linked.Token);
                }

                if (n == 0) break; // EOF

                if (coalesce)
                {
                    // Append to coalesce buffer
                    if (coalesceBuf == null)
                    {
                        coalesceBuf = new byte[n];
                        Buffer.BlockCopy(buf, 0, coalesceBuf, 0, n);
                    }
                    else
                    {
                        var newBuf = new byte[coalesceBuf.Length + n];
                        Buffer.BlockCopy(coalesceBuf, 0, newBuf, 0, coalesceBuf.Length);
                        Buffer.BlockCopy(buf, 0, newBuf, coalesceBuf.Length, n);
                        coalesceBuf = newBuf;
                    }
                    // Flush immediately if buffer is large enough
                    if (coalesceBuf.Length >= 32 * 1024)
                        await FlushCoalesce();
                }
                else
                {
                    var payload = new byte[n];
                    Buffer.BlockCopy(buf, 0, payload, 0, n);
                    var err = await SendToPeerAsync(Protocol.Encode(Protocol.MsgData, state.Id, NextSeq(state.Id), payload));
                    if (err != null) { Log($"Send DATA failed stream={state.Id}: {err}"); return; }
                }
            }
        }
        catch (OperationCanceledException) { }
        catch (IOException) { } // connection closed
        catch (Exception ex) { Log($"TCP read stream={state.Id}: {ex.Message}"); }
        finally
        {
            // Flush any remaining buffered data
            if (coalesce && coalesceBuf != null && coalesceBuf.Length > 0)
            {
                try { await FlushCoalesce(); } catch { }
            }
        }
    }

    // --- Readiness ---

    /// <summary>
    /// Waits until the upstream WS is connected and (in direct mode) peer ID is known.
    /// Returns false if timed out or cancelled.
    /// </summary>
    private async Task<bool> WaitForReadyAsync(CancellationToken ct)
    {
        using var timeout = CancellationTokenSource.CreateLinkedTokenSource(ct);
        timeout.CancelAfter(TimeSpan.FromSeconds(10));

        // Send SYNC to speed up peer discovery when upstream is connected but peer is unknown
        if (_upstreamReady && !Relay && string.IsNullOrEmpty(_peerConnId))
        {
            try
            {
                Log("Peer unknown, sending SYNC for discovery");
                await WsSendUpstream(Protocol.Encode(Protocol.MsgSync, 0), ct);
            }
            catch { }
        }

        try
        {
            while (!timeout.IsCancellationRequested)
            {
                bool peerOk = Relay || !string.IsNullOrEmpty(_peerConnId);
                if (_upstreamReady && peerOk) return true;
                await Task.Delay(200, timeout.Token);
            }
        }
        catch (OperationCanceledException) when (!ct.IsCancellationRequested) { }
        // Log diagnostics on failure
        Log($"WaitForReady failed: upstreamReady={_upstreamReady} peerConnId={(_peerConnId != "" ? "set" : "empty")} relay={Relay}");
        return false;
    }

    // --- Send to peer ---

    private async Task<string?> SendToPeerAsync(byte[] frame)
    {
        if (Relay)
        {
            // Relay mode: send through upstream WS
            if (!_upstreamReady)
                return "upstream not connected (relay)";
            try
            {
                await WsSendUpstream(frame, _cts?.Token ?? CancellationToken.None);
                return null;
            }
            catch (Exception ex) { return ex.Message; }
        }

        // Direct mode: wsSend via YC REST API (gRPC not available from C#/MAUI easily)
        var peer = _peerConnId;
        var token = _iamToken;
        if (string.IsNullOrEmpty(peer) || string.IsNullOrEmpty(token))
            return "no peer connected";

        try
        {
            await WsSendApi(peer, frame, token);
            return null;
        }
        catch (Exception ex)
        {
            // Mark stale and SYNC
            _staleConnId = _peerConnId;
            _peerConnId = "";
            Log($"wsSend failed: {ex.GetType().Name}: {ex.Message} (peer={peer}), marked stale, sending SYNC");
            try
            {
                await WsSendUpstream(Protocol.Encode(Protocol.MsgSync, 0), _cts?.Token ?? CancellationToken.None);
            }
            catch { }
            return ex.Message;
        }
    }

    // --- YC WebSocket management API (REST) ---

    private static readonly HttpClient _http = new() { Timeout = TimeSpan.FromSeconds(10) };

    private async Task WsSendApi(string connId, byte[] data, string iamToken)
    {
        if (Relay)
            throw new InvalidOperationException("BUG: WsSendApi called in relay mode");

        var b64 = Convert.ToBase64String(data);
        var json = $"{{\"data\":\"{b64}\",\"type\":\"BINARY\"}}";
        var url = $"https://apigateway-connections.api.cloud.yandex.net/apigateways/websocket/v1/connections/{Uri.EscapeDataString(connId)}:send";

        using var req = new HttpRequestMessage(HttpMethod.Post, url);
        req.Content = new StringContent(json, System.Text.Encoding.UTF8, "application/json");
        req.Headers.Authorization = new System.Net.Http.Headers.AuthenticationHeaderValue("Bearer", iamToken);

        var resp = await _http.SendAsync(req);
        if (!resp.IsSuccessStatusCode)
        {
            var body = await resp.Content.ReadAsStringAsync();
            throw new Exception($"wsSend {(int)resp.StatusCode}: {body}");
        }
    }

    // --- Upstream WS helpers ---

    private async Task WsSendUpstream(byte[] data, CancellationToken ct)
    {
        var ws = _upstream;
        if (ws == null || ws.State != WebSocketState.Open)
            throw new InvalidOperationException("upstream not connected");

        await _writeLock.WaitAsync(ct);
        try
        {
            await ws.SendAsync(new ArraySegment<byte>(data), WebSocketMessageType.Binary, true, ct);
        }
        finally { _writeLock.Release(); }
    }

    private async Task<byte[]?> WsReceiveWithTimeout(TimeSpan timeout, CancellationToken ct)
    {
        var ws = _upstream;
        if (ws == null) return null;

        using var toCts = CancellationTokenSource.CreateLinkedTokenSource(ct);
        toCts.CancelAfter(timeout);

        var buffer = new byte[128 * 1024];
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
        catch (OperationCanceledException) when (!ct.IsCancellationRequested)
        {
            return null; // timeout
        }
    }

    private async Task PingLoopAsync(CancellationToken ct)
    {
        try
        {
            while (!ct.IsCancellationRequested && _upstream?.State == WebSocketState.Open)
            {
                await Task.Delay(PingIntervalMs, ct);
                await WsSendUpstream(Protocol.Encode(Protocol.MsgPing, 0), ct);
            }
        }
        catch (OperationCanceledException) { }
        catch (Exception ex) { Log($"Ping error: {ex.Message}"); }
    }

    // --- Helpers ---

    private void CloseStream(StreamState state)
    {
        if (_streams.TryRemove(state.Id, out _))
        {
            _streamSeqs.TryRemove(state.Id, out _);
            state.Dispose();
        }
    }

    private void CancelPendingOpens(string reason)
    {
        var count = _pendingOpens.Count;
        foreach (var kvp in _pendingOpens)
            kvp.Value.TrySetCanceled();
        _pendingOpens.Clear();
        if (count > 0)
            Log($"Cancelled {count} pending opens: {reason}");
    }

    private void CloseAllStreams()
    {
        foreach (var kvp in _streams)
        {
            kvp.Value.Dispose();
        }
        _streams.Clear();
        _streamSeqs.Clear();
        CancelPendingOpens("close all");
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

    private uint NextSeq(uint streamId) =>
        _streamSeqs.AddOrUpdate(streamId, 1, (_, old) => old + 1);

    public void Dispose()
    {
        Stop();
        CloseAllStreams();
        CloseUpstream();
        // Do NOT dispose _writeLock — it's reused across start/stop cycles
        // and background threads may still reference it.
    }
}
