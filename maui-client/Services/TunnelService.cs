using System.Collections.Concurrent;
using System.Net;
using System.Net.Sockets;
using System.Net.WebSockets;
using System.Threading.Channels;

namespace BridgeToFreedom.Services;

/// <summary>
/// Outcome of the helper→adapter end-to-end connectivity probe shown in the
/// UI as a status indicator. The probe is fired automatically once the
/// upstream is ready (per upstream cycle).
/// </summary>
public enum ProbeStatus
{
    /// <summary>Not yet attempted, or reset after disconnect.</summary>
    Idle,
    /// <summary>Probe in progress (one or more attempts).</summary>
    Testing,
    /// <summary>Probe completed successfully — wsApi bidirectional data path verified.</summary>
    Ok,
    /// <summary>All probe attempts failed.</summary>
    Failed,
}

/// <summary>
/// Core tunnel service matching the Go helper logic.
/// Manages upstream WS, TCP listener, stream multiplexing, and wsSend via YC gRPC API (or relay).
/// </summary>
public sealed class TunnelService : IDisposable
{
    public event Action<string>? OnLog;
    public event Action? OnStopped;

    /// <summary>
    /// Fires when the connectivity-probe status changes. <c>detail</c> is a
    /// short human-readable message suitable for the status indicator (RTT
    /// on success, last error or attempt count on failure / in progress).
    /// Always invoked on a thread-pool thread — marshal to the UI yourself.
    /// </summary>
    public event Action<ProbeStatus, string>? OnProbeStatusChanged;

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

    // Short ID assigned by the cloud function to this helper (1..255). Stamped
    // into the top byte of every streamID we allocate so the adapter can route
    // per-stream frames back to us even when several helpers share the tunnel.
    // 0 = unassigned / legacy single-helper deployment.
    private byte _helperShortId;

    private readonly ConcurrentDictionary<uint, StreamState> _streams = new();
    private readonly ConcurrentDictionary<uint, TaskCompletionSource<byte[]>> _pendingOpens = new();
    private readonly ConcurrentDictionary<uint, uint> _streamSeqs = new();

    // Per-active-probe inbound DATA/FIN/RST channel, keyed by the probe's
    // stream ID (which has the PROBE bit set). The probe goroutine drains the
    // channel; HandleFrame routes frames here instead of through the regular
    // _streams dispatch (no TcpClient is registered for a probe stream).
    private readonly ConcurrentDictionary<uint, Channel<(byte type, byte[] payload)>> _probeChannels = new();

    // Set to true once a connectivity probe has been launched for the current
    // upstream cycle. Reset to false on upstream disconnect so the next
    // successful HELLO_OK fires a fresh probe (lets the user verify the wsApi
    // data path after every reconnect).
    private volatile bool _probeRanThisCycle;

    // Config
    public string BridgeUrl { get; set; } = "";
    public string AuthToken { get; set; } = "";
    public string ListenAddress { get; set; } = "127.123.45.67";
    public int ListenPort { get; set; } = 5080;
    public bool Relay { get; set; }
    // Periodic PING to refresh IAM token (PONG carries it) and keep the upstream
    // WS alive against APIGW idle timeout. Gated on a known peer in PingLoopAsync,
    // so an idle tunnel waiting for an adapter doesn't spam the cloud function.
    public int PingIntervalMs { get; set; } = 240000;
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

        // Lifecycle flags for graceful half-close.
        public volatile bool OpenConfirmed;     // OPEN_OK received
        public volatile bool RemoteWriteEnded;  // FIN received from peer
        public volatile bool LocalReadEnded;    // local TCP EOF, FIN sent to peer

        // Async writer queue: payloads from the upstream WS to be written to
        // the local TCP socket. Decouples the WS receive loop from local TCP
        // backpressure (a slow local client must NOT stall the receive loop
        // — that would block every other stream and starve OPEN_OK/PONG).
        public readonly Channel<byte[]> WriteQueue;
        public Task? WriterTask;

        // Per-stream reorder buffer for incoming stream frames. Frames from
        // the peer are sent serially per-stream, but the helper→adapter and
        // adapter→helper paths each go through the YC `wsSend` REST API,
        // whose underlying parallel HTTP processing can deliver frames to
        // the recipient WebSocket out of order. Sequence numbers allow us
        // to restore in-order delivery (matches Go adapter's Reorder=true).
        public uint ExpectedRecvSeq = 1;
        public readonly Dictionary<uint, byte[]> ReorderPending = new();
        public readonly object ReorderLock = new();

        public StreamState(uint id, TcpClient client)
        {
            Id = id;
            Client = client;
            NetStream = client.GetStream();
            Cts = new CancellationTokenSource();
            WriteQueue = Channel.CreateUnbounded<byte[]>(new UnboundedChannelOptions
            {
                SingleReader = true,
                SingleWriter = false,
            });
        }

        public void Dispose()
        {
            if (Closed) return;
            Closed = true;
            try { WriteQueue.Writer.TryComplete(); } catch { }
            try { Cts.Cancel(); } catch { }
            try { NetStream.Dispose(); } catch { }
            try { Client.Dispose(); } catch { }
            try { Cts.Dispose(); } catch { }
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
        _helperShortId = 0;
        _upstreamReady = false;
        _probeRanThisCycle = false;
        EmitProbeStatus(ProbeStatus.Idle, "");
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

                var (ownId, peerId, iamToken, helperShortId) = Protocol.DecodeHelloOK(payload);
                _ownConnId = ownId;
                _peerConnId = peerId;
                _iamToken = iamToken;
                _helperShortId = helperShortId;
                _staleConnId = "";
                delay = 1000;

                if (helperShortId != 0)
                    Log($"Authenticated. ownId={Shorten(ownId)} peerId={Shorten(peerId)} shortId={helperShortId}");
                else
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

                // Proactive SYNC: if HELLO_OK didn't include a peer ID, ask
                // the cloud function for it right away. Without this, peer
                // discovery can take up to the periodic ping/sync interval
                // (~30s); with it, peer is typically known after one
                // round-trip (~200ms).
                if (!Relay && string.IsNullOrEmpty(_peerConnId))
                {
                    try
                    {
                        Log("HELLO_OK had no peer; sending proactive SYNC for discovery");
                        await WsSendUpstream(Protocol.Encode(Protocol.MsgSync, 0), ct);
                    }
                    catch (Exception ex) { Log($"Proactive SYNC failed: {ex.Message}"); }
                }

                // Start listener if not already running
                EnsureListenerRunning(ct);

                // DIAG: prove we reach this point and show flag value.
                Log($"[diag] post-listener; probeRan={_probeRanThisCycle} ready={_upstreamReady} peerEmpty={string.IsNullOrEmpty(_peerConnId)}");

                // Kick off a one-shot connectivity probe so the user can
                // verify the wsApi bidirectional data path is actually
                // working, independently of whether the adapter's configured
                // target is reachable.
                if (!_probeRanThisCycle)
                {
                    _probeRanThisCycle = true;
                    Log("Launching connectivity probe...");
                    _ = Task.Run(async () =>
                    {
                        try { await RunProbeAsync(ct); }
                        catch (Exception ex)
                        {
                            Log($"Probe crashed: {ex.GetType().Name}: {ex.Message}");
                            EmitProbeStatus(ProbeStatus.Failed, $"crashed: {ex.GetType().Name}");
                        }
                    }, ct);
                }

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
            _helperShortId = 0;
            _probeRanThisCycle = false;
            EmitProbeStatus(ProbeStatus.Idle, "");
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
                bool oversized = false;
                WebSocketReceiveResult result;
                do
                {
                    if (totalRead >= buffer.Length)
                    {
                        // Message exceeds our buffer. Rather than tear down the
                        // whole upstream (which drops every active stream), drain
                        // the remainder of THIS message and drop just it. Frames
                        // never legitimately exceed the buffer, so this is purely
                        // defensive against a bug or a hostile peer.
                        oversized = true;
                        result = await _upstream!.ReceiveAsync(
                            new ArraySegment<byte>(buffer, 0, buffer.Length), ct);
                    }
                    else
                    {
                        result = await _upstream!.ReceiveAsync(
                            new ArraySegment<byte>(buffer, totalRead, buffer.Length - totalRead), ct);
                        totalRead += result.Count;
                    }
                    if (result.MessageType == WebSocketMessageType.Close) return;
                } while (!result.EndOfMessage);

                if (oversized)
                {
                    Log($"Message too large (>{buffer.Length} bytes), dropped one frame");
                    continue;
                }

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
            var (type, streamId, seqId, payload) = Protocol.Decode(data);

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
                    return;

                case Protocol.MsgPeerGone:
                    if (Relay)
                    {
                        // In relay mode the data path runs through the cloud (our
                        // upstream WS), NOT via direct wsSend to the peer connId.
                        // A PEER_GONE here is usually just a cold cloud instance
                        // answering discovery with "I don't know the adapter yet";
                        // it does NOT mean our streams are dead. Tearing them down
                        // (as direct mode must) would cancel in-flight opens and
                        // kill working streams. A genuinely gone adapter instead
                        // surfaces as per-stream OPEN_FAIL/RST from the cloud.
                        Log("Peer gone (relay mode) — ignoring, data path is via cloud relay");
                        return;
                    }
                    Log($"Peer gone, closing {_streams.Count} streams");
                    _peerConnId = "";
                    CancelPendingOpens("peer gone");
                    CloseAllStreams();
                    return;

                case Protocol.MsgPong:
                    var token = Protocol.DecodePong(payload);
                    if (token != "") _iamToken = token;
                    return;

                case Protocol.MsgPing:
                    // We never answer PINGs (only the cloud function does). A
                    // stray PING can still arrive if an older cloud function
                    // relays a peer's keepalive instead of handling it; ignore
                    // it quietly instead of logging an "unknown frame" warning.
                    // Matches the Go helper/adapter behaviour.
                    return;

                case Protocol.MsgOpenOK:
                case Protocol.MsgOpenFail:
                case Protocol.MsgData:
                case Protocol.MsgFin:
                case Protocol.MsgRst:
                    // Probe streams aren't registered in _streams; they have
                    // their own pendingOpens entry (for OPEN_OK/FAIL) and a
                    // dedicated DATA/FIN/RST channel drained by RunProbeAsync.
                    if (Protocol.IsProbe(streamId))
                    {
                        if (type == Protocol.MsgOpenOK || type == Protocol.MsgOpenFail)
                        {
                            if (_pendingOpens.TryRemove(streamId, out var probeTcs))
                                probeTcs.TrySetResult(data);
                            return;
                        }
                        if (_probeChannels.TryGetValue(streamId, out var pch))
                            pch.Writer.TryWrite((type, payload));
                        return;
                    }
                    if (!_streams.TryGetValue(streamId, out var s))
                    {
                        // OPEN_OK/OPEN_FAIL fallback for legacy code paths: try the pendingOpens
                        // map directly (in case the stream was somehow not pre-registered).
                        if ((type == Protocol.MsgOpenOK || type == Protocol.MsgOpenFail)
                            && _pendingOpens.TryRemove(streamId, out var tcs))
                        {
                            tcs.TrySetResult(data);
                            return;
                        }
                        // Likely a frame for a stream we already closed. Log at debug.
                        Log($"{Protocol.MsgName(type)} for unknown stream={streamId} seq={seqId}, dropping");
                        return;
                    }
                    DeliverStreamFrameOrdered(s, type, seqId, payload, data);
                    return;

                default:
                    Log($"Unknown frame type=0x{type:X2} stream={streamId}");
                    return;
            }
        }
        catch (Exception ex) { Log($"Frame error: {ex.Message}"); }
    }

    /// <summary>
    /// Maximum out-of-order frames buffered per stream while waiting for a
    /// missing SeqID. Reaching this almost certainly means a frame was lost and
    /// the gap will never close, so we reset the stream rather than buffer
    /// forever (unbounded memory + a permanently stalled stream).
    /// </summary>
    private const int MaxReorderPending = 1024;

    /// <summary>
    /// Routes an inbound stream frame through the per-stream reorder buffer.
    /// Frames with seqId == 0 (legacy/uninitialized) are delivered immediately.
    /// Otherwise frames are delivered strictly in ascending seqId order; gaps
    /// are buffered until the missing predecessor arrives.
    /// </summary>
    private void DeliverStreamFrameOrdered(StreamState s, byte type, uint seqId, byte[] payload, byte[] rawData)
    {
        if (seqId == 0)
        {
            DispatchStreamFrame(s, type, payload, rawData);
            return;
        }

        bool overflow = false;
        lock (s.ReorderLock)
        {
            if (seqId == s.ExpectedRecvSeq)
            {
                DispatchStreamFrame(s, type, payload, rawData);
                s.ExpectedRecvSeq++;
                // Drain any consecutive frames previously buffered out-of-order.
                while (s.ReorderPending.TryGetValue(s.ExpectedRecvSeq, out var nextRaw))
                {
                    s.ReorderPending.Remove(s.ExpectedRecvSeq);
                    var (nType, _, _, nPayload) = Protocol.Decode(nextRaw);
                    DispatchStreamFrame(s, nType, nPayload, nextRaw);
                    s.ExpectedRecvSeq++;
                }
            }
            else if (seqId > s.ExpectedRecvSeq)
            {
                // Gap — buffer until the missing predecessor(s) arrive.
                s.ReorderPending[seqId] = rawData;
                var pending = s.ReorderPending.Count;
                if (pending > MaxReorderPending)
                {
                    overflow = true;
                }
                else if (pending > 0 && pending % 50 == 0)
                {
                    Log($"reorder buffer growing stream={s.Id} pending={pending} " +
                        $"expected={s.ExpectedRecvSeq} got={seqId}");
                }
            }
            else
            {
                // seqId < expected: duplicate or stale frame.
                Log($"duplicate/old frame stream={s.Id} seq={seqId} expected={s.ExpectedRecvSeq} type={Protocol.MsgName(type)}");
            }
        }

        if (overflow)
        {
            Log($"reorder overflow stream={s.Id} pending>{MaxReorderPending} (lost frame?), resetting stream");
            // Tell the peer to abort this stream, then tear it down locally.
            try { _ = SendToPeerAsync(Protocol.Encode(Protocol.MsgRst, s.Id, NextSeq(s.Id))); }
            catch { }
            CloseStream(s);
        }
    }

    /// <summary>
    /// Dispatches an in-order stream frame to its handler. Must NOT do any
    /// blocking I/O — DATA payloads are queued for the per-stream writer task
    /// so the upstream WS receive loop is never stalled by a slow local TCP
    /// consumer.
    /// </summary>
    private void DispatchStreamFrame(StreamState s, byte type, byte[] payload, byte[] rawData)
    {
        switch (type)
        {
            case Protocol.MsgOpenOK:
            case Protocol.MsgOpenFail:
                if (_pendingOpens.TryRemove(s.Id, out var tcs))
                    tcs.TrySetResult(rawData);
                else
                    Log($"{Protocol.MsgName(type)} stream={s.Id}: no pending open (already opened or closed)");
                break;

            case Protocol.MsgData:
                if (s.Closed) break;
                if (!s.WriteQueue.Writer.TryWrite(payload))
                {
                    // Channel was completed — peer is sending DATA after we sent/received FIN.
                    // Log and drop; protocol violation but harmless.
                    Log($"DATA after close stream={s.Id} bytes={payload.Length}");
                }
                break;

            case Protocol.MsgFin:
                Log($"FIN stream={s.Id}");
                s.RemoteWriteEnded = true;
                // Signal end-of-stream to writer task. It will drain any
                // buffered DATA, then half-close the local TCP send side so
                // the local client sees EOF on its read.
                s.WriteQueue.Writer.TryComplete();
                break;

            case Protocol.MsgRst:
                Log($"RST stream={s.Id}");
                CloseStream(s);
                break;
        }
    }

    /// <summary>
    /// Per-stream writer task. Drains the inbound write queue and writes
    /// payloads to the local TCP socket. On channel completion (FIN
    /// received), shuts down the local socket's send side so the local
    /// client sees EOF on its read. The stream is fully closed only when
    /// BOTH directions have ended.
    /// </summary>
    private async Task WriterLoopAsync(StreamState s, CancellationToken ct)
    {
        try
        {
            var reader = s.WriteQueue.Reader;
            while (await reader.WaitToReadAsync(ct).ConfigureAwait(false))
            {
                while (reader.TryRead(out var payload))
                {
                    if (s.Closed) return;
                    try
                    {
                        await s.NetStream.WriteAsync(payload, 0, payload.Length, ct).ConfigureAwait(false);
                    }
                    catch (OperationCanceledException) { return; }
                    catch (Exception ex)
                    {
                        Log($"NetStream write stream={s.Id}: {ex.Message}");
                        // Local socket is broken — send RST to peer and close.
                        if (!s.LocalReadEnded)
                        {
                            try { _ = SendToPeerAsync(Protocol.Encode(Protocol.MsgRst, s.Id, NextSeq(s.Id))); }
                            catch { }
                        }
                        CloseStream(s);
                        return;
                    }
                }
            }

            // Channel completed cleanly — FIN received from peer. Half-close
            // the local socket's send side so the local app reads EOF, but
            // leave the receive side open so we can still forward outgoing
            // data until the local app finishes writing.
            try
            {
                s.Client.Client.Shutdown(SocketShutdown.Send);
            }
            catch { /* socket already closed */ }

            // If the local read side has already ended (FIN already sent to
            // peer), both halves are done — fully close.
            if (s.LocalReadEnded)
                CloseStream(s);
        }
        catch (OperationCanceledException) { }
        catch (Exception ex) { Log($"Writer stream={s.Id}: {ex.Message}"); }
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
        var remote = client.Client.RemoteEndPoint?.ToString() ?? "?";

        // Wait until upstream is connected and (in direct mode) peer is known.
        // We must wait BEFORE allocating the stream ID, because in multi-helper
        // mode the cloud function assigns this helper a 1-byte shortId via
        // HELLO_OK; we stamp it into the top byte of streamID so the adapter
        // can route per-stream frames back to us. If we allocated before
        // HELLO_OK, _helperShortId would still be 0 and the adapter wouldn't
        // know whom to route the response to.
        if (!await WaitForReadyAsync(ct))
        {
            Log($"Inbound TCP from {remote}: not ready (no upstream or peer), closing");
            try { client.Close(); } catch { }
            return;
        }

        // In multi-helper mode the cloud function assigns this helper a 1-byte
        // shortId via HELLO_OK; we stamp it into the top byte of streamID so
        // the adapter can route per-stream frames back to us. shortId==0
        // means single-helper / legacy deployment. The local ID is 23 bits
        // (top bit is the PROBE flag, reserved for synthetic probe streams).
        var local = (Interlocked.Increment(ref _nextStreamId) - 1) & Protocol.StreamLocalIDMask;
        var sid = ((uint)_helperShortId << Protocol.StreamHelperShortIDShift) | local;
        if (_helperShortId != 0)
            Log($"New connection remote={remote} stream={sid} (shortId={_helperShortId} local={local})");
        else
            Log($"New connection remote={remote} stream={sid}");

        var state = new StreamState(sid, client);

        // Pre-register the stream BEFORE sending OPEN so that any DATA
        // frames the peer sends immediately after OPEN_OK are not dropped
        // by the receive loop during the brief window between OPEN_OK
        // arriving and this task resuming. Until OPEN_OK is confirmed,
        // queued DATA simply waits for the writer task.
        if (!_streams.TryAdd(sid, state))
        {
            Log($"Stream id collision sid={sid}, aborting");
            state.Dispose();
            return;
        }

        var tcs = new TaskCompletionSource<byte[]>(TaskCreationOptions.RunContinuationsAsynchronously);
        _pendingOpens[sid] = tcs;

        // Writer task has its own long-lived linked CTS (separate from the
        // local TcpReadLoop one below) so it can keep delivering peer→local
        // DATA even after this method returns (graceful half-close).
        var writerCts = CancellationTokenSource.CreateLinkedTokenSource(ct, state.Cts.Token);
        state.WriterTask = Task.Run(async () =>
        {
            try { await WriterLoopAsync(state, writerCts.Token); }
            finally { try { writerCts.Dispose(); } catch { } }
        }, writerCts.Token);

        using var linkedCts = CancellationTokenSource.CreateLinkedTokenSource(ct, state.Cts.Token);

        try
        {
            // (Readiness was already verified before stream-ID allocation;
            // no need to re-check here.)

            // Send OPEN.
            var err = await SendToPeerAsync(Protocol.Encode(Protocol.MsgOpen, sid, NextSeq(sid)));
            if (err != null)
            {
                Log($"OPEN failed stream={sid}: {err}");
                _pendingOpens.TryRemove(sid, out _);
                CloseStream(state);
                return;
            }

            // Wait for OPEN_OK or OPEN_FAIL.
            using var openCts = CancellationTokenSource.CreateLinkedTokenSource(ct);
            openCts.CancelAfter(TimeSpan.FromSeconds(10));
            byte[] resp;
            try { resp = await tcs.Task.WaitAsync(openCts.Token); }
            catch (OperationCanceledException)
            {
                Log($"OPEN timeout stream={sid}");
                CloseStream(state);
                return;
            }
            finally { _pendingOpens.TryRemove(sid, out _); }

            var (type, _, _, _) = Protocol.Decode(resp);
            if (type == Protocol.MsgOpenFail)
            {
                Log($"OPEN rejected stream={sid}");
                CloseStream(state);
                return;
            }

            state.OpenConfirmed = true;
            Log($"Stream opened stream={sid} remote={remote}");

            // Now forward outbound data (local→peer). Inbound data (peer→local)
            // is already being delivered by the writer task we started above.
            await TcpReadLoopAsync(state, linkedCts.Token);
        }
        catch (OperationCanceledException) { }
        catch (Exception ex) { Log($"Stream {sid} error: {ex.Message}"); }
        finally
        {
            // Local TCP read loop has ended (either local EOF or error).
            state.LocalReadEnded = true;

            // Send FIN to peer so the peer's writer can drain and half-close
            // its end. Skip if stream never opened or already closed.
            if (!state.Closed && state.OpenConfirmed)
            {
                try
                {
                    _ = SendToPeerAsync(Protocol.Encode(Protocol.MsgFin, sid, NextSeq(sid)));
                }
                catch { }
            }

            // If the peer has also FIN'd (or stream never opened), close fully.
            // Otherwise leave the stream half-open so the writer task can
            // continue delivering peer→local DATA until peer sends FIN.
            if (state.RemoteWriteEnded || !state.OpenConfirmed)
            {
                CloseStream(state);
            }
            else
            {
                // Half-close: shut down local TCP receive side so the OS stops
                // buffering further local-app sends (we won't read them).
                try { state.Client.Client.Shutdown(SocketShutdown.Receive); } catch { }
                // Stream stays in _streams until writer task observes FIN and
                // closes it.
            }
        }
    }

    private async Task TcpReadLoopAsync(StreamState state, CancellationToken ct)
    {
        var buf = new byte[32 * 1024];
        var coalesce = WriteCoalescing && WriteCoalescingDelayMs > 0;

        // Reusable accumulation buffer for coalescing, grown by doubling and
        // reused across flushes. The old code rebuilt the entire buffer on every
        // append (new byte[old+n] + two copies) — O(n^2) for a busy stream.
        byte[] coalesceBuf = coalesce ? new byte[32 * 1024] : Array.Empty<byte>();
        int coalesceLen = 0;

        async Task<bool> FlushCoalesce()
        {
            if (coalesceLen == 0) return true;
            var data = new byte[coalesceLen];
            Buffer.BlockCopy(coalesceBuf, 0, data, 0, coalesceLen);
            coalesceLen = 0;
            var err = await SendToPeerAsync(Protocol.Encode(Protocol.MsgData, state.Id, NextSeq(state.Id), data));
            if (err != null) { Log($"Send DATA failed stream={state.Id}: {err}"); return false; }
            return true;
        }

        void Append(int n)
        {
            int needed = coalesceLen + n;
            if (coalesceBuf.Length < needed)
            {
                int cap = coalesceBuf.Length == 0 ? 32 * 1024 : coalesceBuf.Length;
                while (cap < needed) cap *= 2;
                Array.Resize(ref coalesceBuf, cap);
            }
            Buffer.BlockCopy(buf, 0, coalesceBuf, coalesceLen, n);
            coalesceLen += n;
        }

        try
        {
            using var linked = CancellationTokenSource.CreateLinkedTokenSource(ct, state.Cts.Token);
            while (!linked.IsCancellationRequested)
            {
                int n;
                if (coalesce && coalesceLen > 0)
                {
                    // We have buffered data. If more is already waiting, grab it
                    // immediately; otherwise wait up to the coalescing delay for
                    // more data, then flush what we have.
                    if (!state.NetStream.DataAvailable)
                    {
                        try { await Task.Delay(WriteCoalescingDelayMs, linked.Token); }
                        catch (OperationCanceledException) { break; }
                        if (!state.NetStream.DataAvailable)
                        {
                            if (!await FlushCoalesce()) return;
                            continue;
                        }
                    }
                    n = await state.NetStream.ReadAsync(buf, 0, buf.Length, linked.Token);
                }
                else
                {
                    // No buffered data (or coalescing off) — block until data arrives.
                    n = await state.NetStream.ReadAsync(buf, 0, buf.Length, linked.Token);
                }

                if (n == 0) break; // EOF

                if (coalesce)
                {
                    Append(n);
                    // Flush immediately if buffer is large enough.
                    if (coalesceLen >= 32 * 1024)
                    {
                        if (!await FlushCoalesce()) return;
                    }
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
            if (coalesce && coalesceLen > 0)
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

    // --- Connectivity probe ---

    /// <summary>How many probe attempts to make before giving up.</summary>
    private const int ProbeMaxAttempts = 3;
    /// <summary>Delay between probe attempts.</summary>
    private static readonly TimeSpan ProbeRetryDelay = TimeSpan.FromSeconds(3);

    private void EmitProbeStatus(ProbeStatus status, string detail)
    {
        try { OnProbeStatusChanged?.Invoke(status, detail); }
        catch { /* never let UI exceptions kill the tunnel */ }
    }

    /// <summary>
    /// Drives the end-to-end connectivity probe with retries. Emits
    /// <see cref="ProbeStatus.Testing"/> while running, then
    /// <see cref="ProbeStatus.Ok"/> on success or
    /// <see cref="ProbeStatus.Failed"/> if all attempts are exhausted.
    /// </summary>
    private async Task RunProbeAsync(CancellationToken ct)
    {
        Log($"Probe: entered (relay={Relay}, helperShortId={_helperShortId}, peer={(_peerConnId != "" ? "set" : "empty")})");
        EmitProbeStatus(ProbeStatus.Testing, $"Testing connection... (1/{ProbeMaxAttempts})");

        string lastError = "";
        for (int attempt = 1; attempt <= ProbeMaxAttempts && !ct.IsCancellationRequested; attempt++)
        {
            if (attempt > 1)
            {
                EmitProbeStatus(ProbeStatus.Testing,
                    $"Retrying... ({attempt}/{ProbeMaxAttempts}) — last: {lastError}");
                try { await Task.Delay(ProbeRetryDelay, ct); }
                catch (OperationCanceledException) { return; }
            }

            var (ok, detail) = await TryProbeOnceAsync(attempt, ct);
            if (ct.IsCancellationRequested) return;

            if (ok)
            {
                EmitProbeStatus(ProbeStatus.Ok, detail);
                return;
            }
            lastError = detail;
        }

        if (!ct.IsCancellationRequested)
        {
            EmitProbeStatus(ProbeStatus.Failed,
                $"Connection test failed after {ProbeMaxAttempts} attempts: {lastError}");
        }
    }

    /// <summary>
    /// Single probe attempt. Returns (true, "rtt=…") on success, otherwise
    /// (false, "&lt;reason&gt;"). Does not modify probe status; the caller
    /// (<see cref="RunProbeAsync"/>) aggregates attempts and decides when
    /// to flip to Ok / Failed.
    /// </summary>
    private async Task<(bool ok, string detail)> TryProbeOnceAsync(int attempt, CancellationToken ct)
    {
        // Wait for upstream readiness; in non-relay mode also wait for peer
        // (i.e. an adapter to have joined this bridge). We deliberately keep
        // this short — if the adapter isn't up we want to fail fast and let
        // the retry loop emit a clearer status, not hang the UI for 20s.
        var waitTimeout = TimeSpan.FromSeconds(7);
        using var waitCts = CancellationTokenSource.CreateLinkedTokenSource(ct);
        waitCts.CancelAfter(waitTimeout);
        try
        {
            while (!waitCts.IsCancellationRequested)
            {
                bool ready = _upstreamReady && (Relay || !string.IsNullOrEmpty(_peerConnId));
                if (ready) break;
                await Task.Delay(200, waitCts.Token);
            }
        }
        catch (OperationCanceledException) when (!ct.IsCancellationRequested)
        {
            // Distinguish the two failure modes so the UI message is
            // actionable. "Upstream not ready" really means the WebSocket
            // to the cloud isn't open yet; "adapter not connected" means
            // we're authenticated with the cloud but no helper-peer
            // (adapter) is currently registered on the other end.
            if (!_upstreamReady)
            {
                Log($"Probe attempt {attempt}: upstream not ready within {waitTimeout.TotalSeconds:F0}s");
                return (false, "upstream not ready");
            }
            Log($"Probe attempt {attempt}: adapter not connected within {waitTimeout.TotalSeconds:F0}s");
            return (false, "adapter not connected");
        }
        catch (OperationCanceledException) { return (false, "cancelled"); }

        if (ct.IsCancellationRequested) return (false, "cancelled");

        // Allocate a probe-flagged stream ID.
        var local = (Interlocked.Increment(ref _nextStreamId) - 1) & Protocol.StreamLocalIDMask;
        var sid = ((uint)_helperShortId << Protocol.StreamHelperShortIDShift)
                  | Protocol.StreamProbeFlag
                  | local;

        var openTcs = new TaskCompletionSource<byte[]>(TaskCreationOptions.RunContinuationsAsynchronously);
        var dataChan = Channel.CreateUnbounded<(byte type, byte[] payload)>(new UnboundedChannelOptions
        {
            SingleReader = true,
            SingleWriter = false,
        });

        _pendingOpens[sid] = openTcs;
        _probeChannels[sid] = dataChan;

        try
        {
            var startUtc = DateTime.UtcNow;
            Log($"Probe attempt {attempt}: sending OPEN stream={sid} (shortId={_helperShortId}, PROBE=1)");

            var openErr = await SendToPeerAsync(Protocol.Encode(Protocol.MsgOpen, sid));
            if (openErr != null)
            {
                Log($"Probe attempt {attempt}: OPEN send failed: {openErr}");
                return (false, $"send failed: {openErr}");
            }

            // Wait for OPEN_OK.
            byte[] resp;
            using (var openTimeout = CancellationTokenSource.CreateLinkedTokenSource(ct))
            {
                openTimeout.CancelAfter(TimeSpan.FromSeconds(10));
                using var reg = openTimeout.Token.Register(() => openTcs.TrySetCanceled());
                try
                {
                    resp = await openTcs.Task.ConfigureAwait(false);
                }
                catch (OperationCanceledException) when (!ct.IsCancellationRequested)
                {
                    Log($"Probe attempt {attempt}: OPEN_OK timeout stream={sid} (10s)");
                    return (false, "OPEN_OK timeout");
                }
                catch (OperationCanceledException) { return (false, "cancelled"); }
            }

            var (rType, _, _, rPayload) = Protocol.Decode(resp);
            if (rType == Protocol.MsgOpenFail)
            {
                var reason = System.Text.Encoding.UTF8.GetString(rPayload);
                Log($"Probe attempt {attempt}: OPEN_FAIL stream={sid} reason={reason}");
                return (false, $"OPEN_FAIL: {reason}");
            }
            Log($"Probe attempt {attempt}: OPEN_OK stream={sid} rtt={(DateTime.UtcNow - startUtc).TotalMilliseconds:F0}ms");

            // Send a token GET so this looks like a real HTTP request on the wire.
            var getReq = System.Text.Encoding.ASCII.GetBytes(
                "GET / HTTP/1.0\r\nHost: probe.bridge-to-freedom\r\nUser-Agent: btf-maui-probe\r\n\r\n");
            var dataErr = await SendToPeerAsync(Protocol.Encode(Protocol.MsgData, sid, payload: getReq));
            if (dataErr != null)
            {
                Log($"Probe attempt {attempt}: GET send failed: {dataErr}");
                return (false, $"GET send failed: {dataErr}");
            }

            // Read response until FIN/RST or timeout.
            using var readCts = CancellationTokenSource.CreateLinkedTokenSource(ct);
            readCts.CancelAfter(TimeSpan.FromSeconds(10));
            var body = new List<byte>(256);
            try
            {
                while (await dataChan.Reader.WaitToReadAsync(readCts.Token))
                {
                    while (dataChan.Reader.TryRead(out var item))
                    {
                        switch (item.type)
                        {
                            case Protocol.MsgData:
                                body.AddRange(item.payload);
                                break;
                            case Protocol.MsgFin:
                                goto done;
                            case Protocol.MsgRst:
                                Log($"Probe attempt {attempt}: RST stream={sid} after {body.Count} bytes");
                                return (false, "RST received");
                        }
                    }
                }
            done:;
            }
            catch (OperationCanceledException) when (!ct.IsCancellationRequested)
            {
                Log($"Probe attempt {attempt}: response timeout stream={sid} after {body.Count} bytes (no FIN in 10s)");
                return (false, "response timeout");
            }
            catch (OperationCanceledException) { return (false, "cancelled"); }

            var totalMs = (DateTime.UtcNow - startUtc).TotalMilliseconds;
            if (body.Count == 0)
            {
                Log($"Probe attempt {attempt}: empty response stream={sid} rtt={totalMs:F0}ms");
                return (false, "empty response");
            }
            const string expected = "HTTP/1.1 200 OK";
            var bodyText = System.Text.Encoding.ASCII.GetString(body.ToArray());
            if (bodyText.StartsWith(expected, StringComparison.Ordinal))
            {
                var msg = $"OK — verified ({totalMs:F0} ms)";
                Log($"Probe attempt {attempt}: OK stream={sid} rtt={totalMs:F0}ms bytes={body.Count}");
                return (true, msg);
            }
            var preview = bodyText.Length > 60 ? bodyText.Substring(0, 60) + "..." : bodyText;
            Log($"Probe attempt {attempt}: unexpected response stream={sid} rtt={totalMs:F0}ms bytes={body.Count} preview={preview}");
            return (false, "unexpected response");
        }
        finally
        {
            _pendingOpens.TryRemove(sid, out _);
            _probeChannels.TryRemove(sid, out _);
            try { dataChan.Writer.TryComplete(); } catch { }
        }
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
                // Only ping while a peer is known (direct mode) or in relay mode
                // where we always need the WS alive for our own data path.
                // Without a peer in direct mode we don't need IAM-token refresh
                // and we don't need to keep the cloud function warm; the WS may
                // idle out and reconnect on demand.
                if (!Relay && string.IsNullOrEmpty(_peerConnId)) continue;
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
