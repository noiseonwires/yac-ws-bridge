// Bridge to Freedom v4 — Yandex Cloud Function
// Discovery service: exchanges connection IDs between adapter and helper.
// Optionally relays helper's stream frames to the adapter (relay mode).
//
// Env: AUTH_TOKEN (required), HTTP_URL (required — full URL of the adapter's
//      HTTP endpoint, e.g. https://your-server:3001/conn-ids, used to recover
//      connection IDs on cold start; the path part is whatever you set in
//      the adapter's http.path config).

const https = require('https');
const http = require('http');
const crypto = require('crypto');

const AUTH_TOKEN = process.env.AUTH_TOKEN;
const HTTP_URL = process.env.HTTP_URL || null;

const httpsAgent = new https.Agent({ keepAlive: true });
const httpAgent = new http.Agent({ keepAlive: true });

// Constant-time auth-token comparison to avoid leaking the shared secret via
// response timing. timingSafeEqual requires equal-length buffers, so compare
// lengths first (the length itself is not secret).
function safeEqual(a, b) {
  const ba = Buffer.from(String(a), 'utf-8');
  const bb = Buffer.from(String(b), 'utf-8');
  if (ba.length !== bb.length) return false;
  return crypto.timingSafeEqual(ba, bb);
}

// Message type names for logging.
const MSG_NAMES = {
  0x01: 'HELLO', 0x02: 'HELLO_OK', 0x03: 'HELLO_ERR',
  0x04: 'PEER_CONN', 0x05: 'PEER_GONE', 0x06: 'SYNC',
  0x10: 'OPEN', 0x11: 'OPEN_OK', 0x12: 'OPEN_FAIL',
  0x20: 'DATA', 0x21: 'FIN', 0x22: 'RST',
  0xF0: 'PING', 0xF1: 'PONG',
};
function msgName(type) { return MSG_NAMES[type] || '0x' + type.toString(16); }

// --- State (local cache per instance) ---
let adapterConnId = null;
// Multi-helper support: each helper connection gets a unique 1-byte short ID
// (1..255). The helper stamps this ID into the top byte of every streamID it
// allocates, so the adapter can route per-stream frames back to the right
// helper. shortID=0 means "unassigned / legacy".
const helpers = new Map();        // connId -> shortId
const usedShortIds = new Set();   // for fast allocation
let _initPromise = null;
let _lastFetchMs = 0; // timestamp of last fetchConnIds call
let _inflightFetch = null; // shared in-flight /conn-ids refresh (coalesces concurrent lookups)

function allocateShortId() {
  for (let i = 1; i <= 255; i++) {
    if (!usedShortIds.has(i)) {
      usedShortIds.add(i);
      return i;
    }
  }
  console.warn('allocateShortId: pool exhausted (>255 helpers)');
  return 0;
}

function rememberHelper(connId) {
  if (!connId) return 0;
  const existing = helpers.get(connId);
  if (existing) return existing;
  const sid = allocateShortId();
  if (sid !== 0) helpers.set(connId, sid);
  return sid;
}

function forgetHelper(connId) {
  const sid = helpers.get(connId);
  if (sid) {
    helpers.delete(connId);
    usedShortIds.delete(sid);
  }
  return sid || 0;
}

// --- Protocol constants ---
const MSG_HELLO     = 0x01;
const MSG_HELLO_OK  = 0x02;
const MSG_HELLO_ERR = 0x03;
const MSG_PEER_CONN = 0x04;
const MSG_PEER_GONE = 0x05;
const MSG_SYNC      = 0x06;
const MSG_PING      = 0xF0;
const MSG_PONG      = 0xF1;
const MSG_OPEN      = 0x10;
const MSG_OPEN_FAIL = 0x12;
const MSG_DATA      = 0x20;
const MSG_FIN       = 0x21;
const MSG_RST       = 0x22;

// --- Helpers ---

function httpGet(url, headers) {
  return new Promise((resolve, reject) => {
    const mod = url.startsWith('https') ? https : http;
    const agent = url.startsWith('https') ? httpsAgent : httpAgent;
    const p = new URL(url);
    const req = mod.request({
      hostname: p.hostname, port: p.port || (p.protocol === 'https:' ? 443 : 80),
      path: p.pathname + p.search, method: 'GET', agent,
      headers: headers || {},
    }, res => { let d = ''; res.on('data', c => d += c); res.on('end', () => resolve({ status: res.statusCode, body: d })); });
    req.on('error', reject);
    // Bound the GET so a slow/hung adapter HTTP endpoint can't wedge the
    // coalesced refresh (every concurrent frame awaiting it would stall).
    req.setTimeout(2500, () => req.destroy(new Error('timeout')));
    req.end();
  });
}

// Fetch connection IDs from adapter on cold start (with retries).
// If iamToken is provided, also pass it to the adapter as X-IAM-Token so the
// adapter can refresh its cached IAM token without needing a PING/PONG round
// trip. The init-time call (no handler context yet) skips this header.
// maxAttempts bounds the internal retry loop; the relay hot path passes 1 for
// a quick single-shot re-learn (it must stay well under the function timeout).
async function fetchConnIds(iamToken, maxAttempts = 3) {
  if (!HTTP_URL) return;
  const url = HTTP_URL;
  const headers = { 'Authorization': 'Bearer ' + AUTH_TOKEN };
  if (iamToken) headers['X-IAM-Token'] = iamToken;
  for (let attempt = 1; attempt <= maxAttempts; attempt++) {
    try {
      console.log(`fetchConnIds attempt=${attempt} url=${url} iamTokenLen=${(iamToken || '').length}`);
      const r = await httpGet(url, headers);
      if (r.status === 200 && r.body) {
        const data = JSON.parse(r.body);
        if (data.adapterConnId) adapterConnId = data.adapterConnId;
        // New format: full helpers array with shortIds.
        if (Array.isArray(data.helpers)) {
          helpers.clear();
          usedShortIds.clear();
          for (const h of data.helpers) {
            if (h && h.connId && h.shortId) {
              helpers.set(h.connId, h.shortId);
              usedShortIds.add(h.shortId);
            }
          }
        } else if (data.helperConnId) {
          // Legacy single-helper response from an older adapter: allocate a
          // shortId locally so multi-helper logic still works downstream.
          if (!helpers.has(data.helperConnId)) rememberHelper(data.helperConnId);
        }
        console.log(`fetchConnIds OK adapter=${adapterConnId || 'null'} helpers=${helpers.size}`);
        return;
      }
      console.log(`fetchConnIds attempt=${attempt} status=${r.status} body=${(r.body || '').substring(0, 100)}`);
    } catch (e) {
      console.error(`fetchConnIds attempt=${attempt} err=${e.message || e}`);
    }
    // Wait before retry (500ms, 1s)
    if (attempt < maxAttempts) await new Promise(r => setTimeout(r, attempt * 500));
  }
  console.warn('fetchConnIds: all retries exhausted, proceeding without conn-ids');
}

// Coalesced /conn-ids refresh. Concurrent callers share ONE in-flight GET and
// all await its result. This is the key relay-mode fix: in relay mode every
// data frame needs the cloud instance to know a live adapterConnId, but with
// serverless concurrency a cold/parallel instance receives a burst of frames
// at once. The previous per-call throttle let only the first frame fetch and
// made the rest return early with adapterConnId still null — surfacing as
// "no adapter connected" RSTs even though the adapter was up. Coalescing lets
// every racing frame await the same lookup and then succeed.
function refreshConnIds(iamToken, maxAttempts = 1) {
  if (_inflightFetch) return _inflightFetch;
  _lastFetchMs = Date.now();
  _inflightFetch = fetchConnIds(iamToken, maxAttempts)
    .catch(e => { console.error(`refreshConnIds err=${(e && e.message) || e}`); })
    .finally(() => { _inflightFetch = null; });
  return _inflightFetch;
}

_initPromise = fetchConnIds();

// Ensure the local cache knows the peer of interest, refreshing from the
// adapter's HTTP endpoint if needed. Concurrent callers coalesce onto a single
// in-flight refresh (see refreshConnIds). Passes the current IAM token along so
// the adapter can refresh its cached one. For helpers, an optional
// `requiredHelperConnId` makes the check helper-specific: even when other
// helpers are already known, we still re-fetch if this particular connId is
// missing. That matters for serverless cold starts where a MESSAGE for a new
// helper may hit an instance warmed by an unrelated helper — without
// re-syncing we could allocate a shortId that collides with one the adapter
// already has for someone else.
async function ensurePeerKnown(which, iamToken, requiredHelperConnId = null) {
  if (which === 'adapter' && adapterConnId) return;
  if (which === 'helper') {
    if (requiredHelperConnId) {
      if (helpers.has(requiredHelperConnId)) return;
    } else if (helpers.size > 0) {
      return;
    }
  }
  // Always refresh when the peer is unknown. There is NO time-based throttle:
  // refreshConnIds already coalesces concurrent callers onto a single in-flight
  // GET, so a burst of frames can't cause a fetch storm, and relay correctness
  // depends on actually fetching when we have no peer to deliver to. The old
  // "skip if fetched in the last N ms" guard is exactly what left cold/parallel
  // instances with adapterConnId=null and produced spurious "adapter
  // unreachable" RSTs in relay mode.
  console.log(`ensurePeerKnown: ${which} unknown (required=${requiredHelperConnId || 'any'}), refreshing conn-ids`);
  await refreshConnIds(iamToken, requiredHelperConnId ? 2 : 3);
}

// WS management API — send binary data to a connection.
async function wsSend(connId, data, token) {
  const b64 = Buffer.from(data).toString('base64');
  const body = JSON.stringify({ data: b64, type: 'BINARY' });
  const start = Date.now();
  return new Promise((resolve, reject) => {
    const req = https.request({
      hostname: 'apigateway-connections.api.cloud.yandex.net',
      path: `/apigateways/websocket/v1/connections/${encodeURIComponent(connId)}:send`,
      method: 'POST', agent: httpsAgent,
      headers: { 'Content-Type': 'application/json', 'Authorization': 'Bearer ' + token, 'Content-Length': Buffer.byteLength(body) },
    }, res => {
      let d = '';
      res.on('data', c => d += c);
      res.on('end', () => {
        const ms = Date.now() - start;
        if (res.statusCode >= 300) {
          console.error(`wsSend FAIL connId=${connId} status=${res.statusCode} ms=${ms} body=${d.substring(0, 200)}`);
        } else {
          console.log(`wsSend OK connId=${connId} bytes=${data.length} ms=${ms}`);
        }
        resolve(res.statusCode);
      });
    });
    req.on('error', e => { console.error(`wsSend ERR connId=${connId} err=${e.message} ms=${Date.now() - start}`); resolve(500); });
    // Bound the call so a hung YC API request can't burn the whole function
    // execution timeout (and so the relay retry path can react promptly).
    req.setTimeout(3000, () => { console.error(`wsSend TIMEOUT connId=${connId} ms=${Date.now() - start}`); req.destroy(new Error('timeout')); });
    req.write(body);
    req.end();
  });
}

function sleep(ms) { return new Promise(r => setTimeout(r, ms)); }

// True for wsSend HTTP statuses that mean "this connectionId is gone" (the
// adapter closed/reconnected), as opposed to transient errors (429 rate limit,
// 5xx, our 500 timeout sentinel) where the SAME connId is still valid and we
// should just retry rather than rediscover.
function connIdGone(status) { return status === 400 || status === 404 || status === 410; }

// Relay a single helper stream-frame to the adapter, resiliently.
// Returns null on success, or a binary RST/OPEN_FAIL frame to send back to the
// helper only when the adapter is genuinely unreachable.
//
// Two failure modes this must survive, because APIGW spreads ONE helper's
// frames across MANY serverless instances (each with its own module-global
// adapterConnId):
//   1. Cold instance: adapterConnId is unknown -> fetch it from /conn-ids.
//   2. Stale instance: adapterConnId is cached but the adapter reconnected with
//      a NEW connId, so wsSend to the old id fails.
//
// The cure for BOTH is the same: on any failure, RE-FETCH the authoritative id
// from the adapter's /conn-ids. fetchConnIds only overwrites adapterConnId on a
// real HTTP 200, so a failed fetch never corrupts it. Crucially we NEVER set
// adapterConnId=null here: this global is shared by every concurrent invocation
// on the instance, and nulling it on one frame's hiccup cascades "unreachable"
// to all the others (the collapse seen in the logs while the adapter was up).
async function relayToAdapter(buf, type, streamId, token) {
  const deadline = Date.now() + 8500; // stay under the 10s function timeout
  let healed = false;                 // re-fetched a fresh id at least once
  for (let attempt = 1; attempt <= 4 && Date.now() < deadline; attempt++) {
    if (!adapterConnId) {
      await refreshConnIds(token, 1);
      if (!adapterConnId) {
        // Cold instance and the fetch didn't land yet; pause and retry it.
        if (Date.now() < deadline) await sleep(300);
        continue;
      }
    }
    const id = adapterConnId;
    const st = await wsSend(id, buf, token);
    if (st < 400) {
      if (attempt > 1) console.log(`relay ${msgName(type)} stream=${streamId} -> adapter ${id} OK (attempt ${attempt})`);
      return null;
    }
    console.error(`relay ${msgName(type)} stream=${streamId} status=${st} attempt=${attempt} id=${id}`);
    // Re-fetch the authoritative id once. If it changed, the cached id was
    // stale (adapter reconnected) -> retry the fresh id immediately. If it's
    // unchanged, the adapter is healthy and this was a transient API error ->
    // pause and retry the same id.
    if (!healed && Date.now() < deadline) {
      healed = true;
      const before = adapterConnId;
      await refreshConnIds(token, 1);
      if (adapterConnId && adapterConnId !== before) {
        console.log(`relay ${msgName(type)} stream=${streamId}: healed adapter id ${before} -> ${adapterConnId}, retrying`);
        continue;
      }
    }
    if (Date.now() < deadline) await sleep(300);
  }
  console.warn(`relay ${msgName(type)} stream=${streamId}: adapter unreachable after retries, signalling helper`);
  return type === MSG_OPEN
    ? binaryResp(encodeStreamFrame(MSG_OPEN_FAIL, streamId, Buffer.from('adapter unreachable')))
    : binaryResp(encodeStreamFrame(MSG_RST, streamId));
}

// Send the same frame to many helper connections in parallel, dropping any the
// WS API reports as stale (status >= 400). Parallel fan-out bounds the total
// time by the slowest single wsSend instead of their sum, which matters
// against the function execution timeout when several helpers are connected.
async function notifyHelpers(connIds, frame, token, label) {
  if (!connIds.length) return;
  const results = await Promise.all(connIds.map(id =>
    wsSend(id, frame, token).then(st => ({ id, st }))));
  for (const { id, st } of results) {
    // Only drop a helper when the API says its connection is gone. A transient
    // 429/5xx must NOT evict a healthy helper (that would churn its shortId and
    // disrupt its live streams) — same shared-state hazard as the adapter side.
    if (connIdGone(st)) {
      console.log(`${label}: helper ${id} is gone (status=${st}), dropping`);
      forgetHelper(id);
    } else if (st >= 400) {
      console.log(`${label}: transient wsSend to helper ${id} (status=${st}), keeping`);
    }
  }
}

// --- Protocol encode helpers ---

// Frame: [1B type][4B streamID=0][4B seqID=0][payload]
function encodeControl(type, payload) {
  const p = payload || Buffer.alloc(0);
  const buf = Buffer.alloc(9 + p.length);
  buf[0] = type;
  buf.writeUInt32BE(0, 1); // streamID = 0
  buf.writeUInt32BE(0, 5); // seqID = 0
  p.copy(buf, 9);
  return buf;
}

// HELLO_OK payload: [2B ownIdLen][ownId][2B peerIdLen][peerId][2B tokenLen][token][1B helperShortId?]
// helperShortID is appended only when nonzero (multi-helper mode).
function encodeHelloOK(ownId, peerId, iamToken, helperShortId = 0) {
  const o = Buffer.from(ownId || '', 'utf-8');
  const p = Buffer.from(peerId || '', 'utf-8');
  const t = Buffer.from(iamToken || '', 'utf-8');
  const extra = helperShortId ? 1 : 0;
  const buf = Buffer.alloc(2 + o.length + 2 + p.length + 2 + t.length + extra);
  let off = 0;
  buf.writeUInt16BE(o.length, off); off += 2; o.copy(buf, off); off += o.length;
  buf.writeUInt16BE(p.length, off); off += 2; p.copy(buf, off); off += p.length;
  buf.writeUInt16BE(t.length, off); off += 2; t.copy(buf, off); off += t.length;
  if (extra) buf[off] = helperShortId;
  return buf;
}

// PEER_CONN payload: [2B peerIdLen][peerId][2B tokenLen][token][1B helperShortId?]
// helperShortID is appended in the cloud-function -> adapter direction so the
// adapter knows which helper is announcing itself. Omitted in the
// cloud-function -> helper direction (single adapter, no shortID needed).
function encodePeerConn(peerId, iamToken, helperShortId = 0) {
  const p = Buffer.from(peerId || '', 'utf-8');
  const t = Buffer.from(iamToken || '', 'utf-8');
  const extra = helperShortId ? 1 : 0;
  const buf = Buffer.alloc(2 + p.length + 2 + t.length + extra);
  let off = 0;
  buf.writeUInt16BE(p.length, off); off += 2; p.copy(buf, off); off += p.length;
  buf.writeUInt16BE(t.length, off); off += 2; t.copy(buf, off); off += t.length;
  if (extra) buf[off] = helperShortId;
  return buf;
}

// PONG payload: [2B tokenLen][token]
function encodePong(iamToken) {
  const t = Buffer.from(iamToken || '', 'utf-8');
  const buf = Buffer.alloc(2 + t.length);
  buf.writeUInt16BE(t.length, 0);
  t.copy(buf, 2);
  return buf;
}

// Encode a stream-level frame (OPEN_FAIL, RST, etc.) with a specific streamId.
function encodeStreamFrame(type, streamId, payload) {
  const p = payload || Buffer.alloc(0);
  const buf = Buffer.alloc(9 + p.length);
  buf[0] = type;
  buf.writeUInt32BE(streamId, 1);
  buf.writeUInt32BE(0, 5); // seqID = 0
  p.copy(buf, 9);
  return buf;
}

function binaryResp(buf) {
  return { statusCode: 200, headers: { 'Content-Type': 'application/octet-stream' }, body: buf.toString('base64'), isBase64Encoded: true };
}

// --- Handler ---
module.exports.handler = async function (event, context) {
  try { return await handle(event, context); }
  catch (e) { console.error('ERROR:', e.stack || e); return { statusCode: 200 }; }
};

async function handle(event, context) {
  if (_initPromise) { await _initPromise; _initPromise = null; }

  const rc = event.requestContext || {};
  const connId = rc.connectionId;
  const ev = rc.eventType;
  const token = context.token?.access_token || '';
  const route = rc.apiGateway?.operationContext?.route || '';

  console.log(`event route=${route} type=${ev} connId=${connId} state=[adapter=${adapterConnId || 'null'} helpers=${helpers.size}]`);

  // --- ADAPTER ---
  if (route === 'adapter') {
    if (ev === 'CONNECT') {
      adapterConnId = connId;
      console.log(`adapter CONNECT connId=${connId}`);
      return { statusCode: 200 };
    }
    if (ev === 'MESSAGE') {
      if (!adapterConnId) { adapterConnId = connId; console.log(`adapter connId recovered from message: ${connId}`); }

      const buf = event.isBase64Encoded ? Buffer.from(event.body, 'base64') : Buffer.from(event.body || '');
      if (buf.length < 9) { console.warn(`adapter MESSAGE too short len=${buf.length}`); return { statusCode: 200 }; }
      const type = buf[0];
      const streamId = buf.readUInt32BE(1);
      const seqId = buf.readUInt32BE(5);
      console.log(`adapter MESSAGE type=${msgName(type)} streamId=${streamId} seq=${seqId} len=${buf.length}`);

      if (type === MSG_HELLO) {
        const ver = buf[9];
        const tok = buf.subarray(10).toString('utf-8');
        const tokenOk = safeEqual(tok, AUTH_TOKEN);
        if (ver !== 1 || !tokenOk) {
          console.error(`adapter HELLO auth failed ver=${ver} tokenMatch=${tokenOk}`);
          return binaryResp(encodeControl(MSG_HELLO_ERR, Buffer.from('auth failed')));
        }
        if (helpers.size === 0) await ensurePeerKnown('helper', token);
        // Notify EVERY known helper that the adapter is here (in parallel).
        await notifyHelpers(
          Array.from(helpers).map(([hConnId]) => hConnId),
          encodeControl(MSG_PEER_CONN, encodePeerConn(adapterConnId, token, 0)),
          token, 'adapter HELLO');
        console.log(`adapter authenticated connId=${adapterConnId} helpers=${helpers.size} tokenLen=${token.length}`);
        // Adapter HELLO_OK no longer carries a peerId (multi-helper). The
        // adapter learns each helper via separate PEER_CONN frames pushed via
        // wsSend above.
        return binaryResp(encodeControl(MSG_HELLO_OK, encodeHelloOK(adapterConnId, '', token, 0)));
      }
      if (type === MSG_PING) {
        // Re-learn adapter connId from this message
        if (adapterConnId !== connId) {
          console.log(`adapter PING: re-learned adapterConnId=${connId} (was ${adapterConnId || 'null'})`);
          adapterConnId = connId;
        }
        if (helpers.size === 0) await ensurePeerKnown('helper', token);
        // Cross-notify each helper of the adapter (covers cross-instance state
        // loss), in parallel.
        await notifyHelpers(
          Array.from(helpers).map(([hConnId]) => hConnId),
          encodeControl(MSG_PEER_CONN, encodePeerConn(adapterConnId, token, 0)),
          token, 'adapter PING');
        console.log(`adapter PING -> PONG tokenLen=${token.length}`);
        return binaryResp(encodeControl(MSG_PONG, encodePong(token)));
      }
      if (type === MSG_SYNC) {
        if (helpers.size === 0) await ensurePeerKnown('helper', token);
        if (helpers.size === 0) {
          console.log('adapter SYNC -> PEER_GONE (no helpers)');
          return binaryResp(encodeControl(MSG_PEER_GONE));
        }
        // Announce every known helper to the adapter so it can rebuild its
        // routing table. Return the first entry as the SYNC response and push
        // the second-and-later entries to the adapter in parallel.
        const entries = Array.from(helpers); // [[connId, shortId], ...]
        const [firstConnId, firstShort] = entries[0];
        const first = encodeControl(MSG_PEER_CONN, encodePeerConn(firstConnId, token, firstShort));
        const rest = entries.slice(1);
        if (rest.length) {
          const statuses = await Promise.all(rest.map(([hConnId, sid]) => {
            console.log(`adapter SYNC: pushing PEER_CONN helper=${hConnId} shortId=${sid}`);
            return wsSend(adapterConnId, encodeControl(MSG_PEER_CONN, encodePeerConn(hConnId, token, sid)), token);
          }));
          if (statuses.some(connIdGone)) {
            console.log(`adapter SYNC: adapter ${adapterConnId} gone, clearing`);
            adapterConnId = null;
          }
        }
        console.log(`adapter SYNC -> PEER_CONN helper=${firstConnId} shortId=${firstShort} (plus ${rest.length} pushed)`);
        return binaryResp(first);
      }
      console.warn(`adapter MESSAGE: unhandled type=${msgName(type)}`);
      return { statusCode: 200 };
    }
    if (ev === 'DISCONNECT') {
      const wasKnown = adapterConnId === connId;
      if (wasKnown) adapterConnId = null;
      console.log(`adapter DISCONNECT connId=${connId} wasKnown=${wasKnown}`);
      // Notify every helper that the adapter is gone (no shortId payload —
      // it's the singleton adapter going away). Fan out in parallel.
      await Promise.all(Array.from(helpers).map(([hConnId]) => {
        console.log(`adapter DISCONNECT: notifying helper ${hConnId} of PEER_GONE`);
        return wsSend(hConnId, encodeControl(MSG_PEER_GONE), token);
      }));
      return { statusCode: 200 };
    }
    console.warn(`adapter: unknown event type=${ev}`);
    return { statusCode: 200 };
  }

  // --- HELPER ---
  if (route === 'helper') {
    if (ev === 'CONNECT') {
      // Don't allocate a shortId yet. The helper hasn't authenticated; we
      // defer allocation until the first MESSAGE (typically HELLO) so that
      // we can first sync from the adapter's /conn-ids and avoid picking a
      // shortId that's already in use elsewhere.
      console.log(`helper CONNECT connId=${connId} (shortId allocation deferred)`);
      return { statusCode: 200 };
    }
    if (ev === 'MESSAGE') {
      // Lazy registration in case CONNECT was processed by a different
      // function instance (state is per-instance for serverless). Before
      // allocating a fresh shortId, sync from the adapter's /conn-ids so we
      // don't reuse a shortId that the adapter (or another instance) already
      // assigned elsewhere.
      let shortId = helpers.get(connId);
      if (!shortId) {
        await ensurePeerKnown('helper', token, connId);
        shortId = helpers.get(connId);
        if (!shortId) {
          shortId = rememberHelper(connId);
          console.log(`helper connId=${connId} recovered from message (new shortId=${shortId}, usedSoFar=${usedShortIds.size})`);
        } else {
          console.log(`helper connId=${connId} recovered from /conn-ids shortId=${shortId}`);
        }
      }

      const buf = event.isBase64Encoded ? Buffer.from(event.body, 'base64') : Buffer.from(event.body || '');
      if (buf.length < 9) { console.warn(`helper MESSAGE too short len=${buf.length}`); return { statusCode: 200 }; }
      const type = buf[0];
      const streamId = buf.readUInt32BE(1);
      const seqId = buf.readUInt32BE(5);
      console.log(`helper MESSAGE type=${msgName(type)} streamId=${streamId} seq=${seqId} len=${buf.length}`);

      // Stream frames (0x10..0x22): relay to adapter (with recovery retry).
      // Use an explicit range — NOT `type >= 0x10` — because the control frames
      // PING (0xF0) and PONG (0xF1) are numerically ABOVE 0x10 too. A naive
      // lower-bound check relayed the helper's keepalive PING to the adapter
      // (which then logged "unknown frame type=0xf0") and burned a wsSend per
      // ping. Control frames (HELLO/PING/SYNC) must fall through to be handled.
      if (type >= MSG_OPEN && type <= MSG_RST) {
        const errResp = await relayToAdapter(buf, type, streamId, token);
        if (errResp) return errResp;
        return { statusCode: 200 };
      }

      if (type === MSG_HELLO) {
        const ver = buf[9];
        const tok = buf.subarray(10).toString('utf-8');
        const tokenOk = safeEqual(tok, AUTH_TOKEN);
        if (ver !== 1 || !tokenOk) {
          console.error(`helper HELLO auth failed ver=${ver} tokenMatch=${tokenOk}`);
          return binaryResp(encodeControl(MSG_HELLO_ERR, Buffer.from('auth failed')));
        }
        if (!adapterConnId) await ensurePeerKnown('adapter', token);
        if (adapterConnId) {
          console.log(`helper HELLO: notifying adapter ${adapterConnId} of helper connId shortId=${shortId}`);
          let st = await wsSend(adapterConnId, encodeControl(MSG_PEER_CONN, encodePeerConn(connId, token, shortId)), token);
          if (st >= 400) {
            // Cached adapter id may be stale. Re-fetch the authoritative id
            // (never null it — that cascades to concurrent invocations) and
            // re-push once so the adapter reliably learns this helper.
            console.log(`helper HELLO: wsSend to adapter ${adapterConnId} failed (status=${st}), re-fetching`);
            const before = adapterConnId;
            await refreshConnIds(token, 1);
            if (adapterConnId && adapterConnId !== before) {
              st = await wsSend(adapterConnId, encodeControl(MSG_PEER_CONN, encodePeerConn(connId, token, shortId)), token);
              console.log(`helper HELLO: re-pushed to healed adapter ${adapterConnId} status=${st}`);
            }
          }
        }
        console.log(`helper authenticated connId=${connId} shortId=${shortId} adapterConnId=${adapterConnId || 'null'} tokenLen=${token.length}`);
        // HELLO_OK back to helper: include this helper's assigned shortId so
        // it can stamp it into the top byte of every streamID it allocates.
        return binaryResp(encodeControl(MSG_HELLO_OK, encodeHelloOK(connId, adapterConnId, token, shortId)));
      }
      if (type === MSG_PING) {
        // Re-learn helper connId from this message (idempotent in multi-helper mode).
        if (!helpers.has(connId)) {
          shortId = rememberHelper(connId);
          console.log(`helper PING: re-learned helper connId=${connId} shortId=${shortId}`);
        }
        if (!adapterConnId) await ensurePeerKnown('adapter', token);
        // Cross-notify adapter of this helper (covers cross-instance state loss).
        if (adapterConnId) {
          console.log(`helper PING: cross-notifying adapter ${adapterConnId} of helper shortId=${shortId}`);
          let st = await wsSend(adapterConnId, encodeControl(MSG_PEER_CONN, encodePeerConn(connId, token, shortId)), token);
          if (st >= 400) {
            // Cached adapter id may be stale. Re-fetch (never null) and re-push.
            console.log(`helper PING: wsSend to adapter ${adapterConnId} failed (status=${st}), re-fetching`);
            const before = adapterConnId;
            await refreshConnIds(token, 1);
            if (adapterConnId && adapterConnId !== before) {
              st = await wsSend(adapterConnId, encodeControl(MSG_PEER_CONN, encodePeerConn(connId, token, shortId)), token);
              console.log(`helper PING: re-pushed to healed adapter ${adapterConnId} status=${st}`);
            }
          }
        }
        console.log(`helper PING -> PONG tokenLen=${token.length}`);
        return binaryResp(encodeControl(MSG_PONG, encodePong(token)));
      }
      if (type === MSG_SYNC) {
        if (!adapterConnId) await ensurePeerKnown('adapter', token);
        if (adapterConnId) {
          console.log(`helper SYNC -> PEER_CONN adapterConnId=${adapterConnId}`);
          // No shortId in this direction (singleton adapter).
          return binaryResp(encodeControl(MSG_PEER_CONN, encodePeerConn(adapterConnId, token, 0)));
        }
        console.log('helper SYNC -> PEER_GONE (no adapter)');
        return binaryResp(encodeControl(MSG_PEER_GONE));
      }
      console.warn(`helper MESSAGE: unhandled type=${msgName(type)}`);
      return { statusCode: 200 };
    }
    if (ev === 'DISCONNECT') {
      const sid = forgetHelper(connId);
      console.log(`helper DISCONNECT connId=${connId} shortId=${sid} remaining=${helpers.size}`);
      if (sid && adapterConnId) {
        console.log(`helper DISCONNECT: notifying adapter ${adapterConnId} of PEER_GONE shortId=${sid}`);
        await wsSend(adapterConnId, encodeControl(MSG_PEER_GONE, Buffer.from([sid])), token);
      }
      return { statusCode: 200 };
    }
    console.warn(`helper: unknown event type=${ev}`);
    return { statusCode: 200 };
  }

  console.warn(`unknown route=${route} event=${ev} connId=${connId}`);
  return { statusCode: 200 };
}
