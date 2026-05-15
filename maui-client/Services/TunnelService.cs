using System.Collections.Concurrent;
using System.Net;
using System.Net.Sockets;
using System.Net.WebSockets;
using System.Threading.Channels;

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
                    Log($"Peer gone, closing {_streams.Count} streams");
                    _peerConnId = "";
                    CancelPendingOpens("peer gone");
                    CloseAllStreams();
                    return;

                case Protocol.MsgPong:
                    var token = Protocol.DecodePong(payload);
                    if (token != "") _iamToken = token;
                    return;

                case Protocol.MsgOpenOK:
                case Protocol.MsgOpenFail:
                case Protocol.MsgData:
                case Protocol.MsgFin:
                case Protocol.MsgRst:
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
                if (pending > 0 && pending % 50 == 0)
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
        var sid = Interlocked.Increment(ref _nextStreamId) - 1;
        var remote = client.Client.RemoteEndPoint?.ToString() ?? "?";
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
            // Wait until upstream is connected and peer is known (up to 10s).
            if (!await WaitForReadyAsync(ct))
            {
                Log($"OPEN failed stream={sid}: not ready (no upstream or peer)");
                _pendingOpens.TryRemove(sid, out _);
                CloseStream(state);
                return;
            }

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
