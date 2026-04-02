// Bridge to Freedom v4 — Yandex Cloud Function
// Discovery service: exchanges connection IDs between adapter and helper.
// Optionally relays helper's QUIC packets to the adapter (relay mode).
//
// Env: AUTH_TOKEN (required), ADAPTER_URL (required — for /conn-ids fetch on init)

const https = require('https');
const http = require('http');

const AUTH_TOKEN = process.env.AUTH_TOKEN;
const ADAPTER_URL = process.env.ADAPTER_URL || null;

const httpsAgent = new https.Agent({ keepAlive: true });
const httpAgent = new http.Agent({ keepAlive: true });

// Message type names for logging.
const MSG_NAMES = {
  0x01: 'HELLO', 0x02: 'HELLO_OK', 0x03: 'HELLO_ERR',
  0x04: 'PEER_CONN', 0x05: 'PEER_GONE', 0x06: 'SYNC',
  0x30: 'QUIC', 0x31: 'QUIC_BATCH',
  0xF0: 'PING', 0xF1: 'PONG',
};
function msgName(type) { return MSG_NAMES[type] || '0x' + type.toString(16); }

// --- State (local cache per instance) ---
let adapterConnId = null;
let helperConnId = null;
let _initPromise = null;
let _lastFetchMs = 0; // timestamp of last fetchConnIds call

// --- Protocol constants ---
const MSG_HELLO     = 0x01;
const MSG_HELLO_OK  = 0x02;
const MSG_HELLO_ERR = 0x03;
const MSG_PEER_CONN = 0x04;
const MSG_PEER_GONE = 0x05;
const MSG_SYNC      = 0x06;
const MSG_QUIC      = 0x30;
const MSG_QUIC_BATCH = 0x31;
const MSG_PING      = 0xF0;
const MSG_PONG      = 0xF1;

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
    req.end();
  });
}

// Fetch connection IDs from adapter on cold start (with retries).
async function fetchConnIds() {
  if (!ADAPTER_URL) return;
  const url = ADAPTER_URL.replace(/\/+$/, '') + '/conn-ids';
  for (let attempt = 1; attempt <= 3; attempt++) {
    try {
      console.log(`fetchConnIds attempt=${attempt} url=${url}`);
      const r = await httpGet(url, { 'Authorization': 'Bearer ' + AUTH_TOKEN });
      if (r.status === 200 && r.body) {
        const data = JSON.parse(r.body);
        if (data.adapterConnId) adapterConnId = data.adapterConnId;
        if (data.helperConnId) helperConnId = data.helperConnId;
        console.log(`fetchConnIds OK adapter=${adapterConnId || 'null'} helper=${helperConnId || 'null'}`);
        return;
      }
      console.log(`fetchConnIds attempt=${attempt} status=${r.status} body=${(r.body || '').substring(0, 100)}`);
    } catch (e) {
      console.error(`fetchConnIds attempt=${attempt} err=${e.message || e}`);
    }
    // Wait before retry (500ms, 1s)
    if (attempt < 3) await new Promise(r => setTimeout(r, attempt * 500));
  }
  console.warn('fetchConnIds: all retries exhausted, proceeding without conn-ids');
}

_initPromise = fetchConnIds();

// Try to learn the missing peer's connId via the adapter HTTP endpoint.
// Skips if called within the last 2 seconds to avoid hammering.
async function ensurePeerKnown(which) {
  const now = Date.now();
  if (now - _lastFetchMs < 2000) return;
  if (which === 'adapter' && adapterConnId) return;
  if (which === 'helper' && helperConnId) return;
  console.log(`ensurePeerKnown: ${which} unknown, calling fetchConnIds`);
  _lastFetchMs = now;
  await fetchConnIds();
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
    req.write(body);
    req.end();
  });
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

// HELLO_OK payload: [2B ownIdLen][ownId][2B peerIdLen][peerId][2B tokenLen][token]
function encodeHelloOK(ownId, peerId, iamToken) {
  const o = Buffer.from(ownId || '', 'utf-8');
  const p = Buffer.from(peerId || '', 'utf-8');
  const t = Buffer.from(iamToken || '', 'utf-8');
  const buf = Buffer.alloc(2 + o.length + 2 + p.length + 2 + t.length);
  let off = 0;
  buf.writeUInt16BE(o.length, off); off += 2; o.copy(buf, off); off += o.length;
  buf.writeUInt16BE(p.length, off); off += 2; p.copy(buf, off); off += p.length;
  buf.writeUInt16BE(t.length, off); off += 2; t.copy(buf, off);
  return buf;
}

// PEER_CONN payload: [2B peerIdLen][peerId][2B tokenLen][token]
function encodePeerConn(peerId, iamToken) {
  const p = Buffer.from(peerId || '', 'utf-8');
  const t = Buffer.from(iamToken || '', 'utf-8');
  const buf = Buffer.alloc(2 + p.length + 2 + t.length);
  let off = 0;
  buf.writeUInt16BE(p.length, off); off += 2; p.copy(buf, off); off += p.length;
  buf.writeUInt16BE(t.length, off); off += 2; t.copy(buf, off);
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

  console.log(`event route=${route} type=${ev} connId=${connId} state=[adapter=${adapterConnId || 'null'} helper=${helperConnId || 'null'}]`);

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
        if (ver !== 1 || tok !== AUTH_TOKEN) {
          console.error(`adapter HELLO auth failed ver=${ver} tokenMatch=${tok === AUTH_TOKEN}`);
          return binaryResp(encodeControl(MSG_HELLO_ERR, Buffer.from('auth failed')));
        }
        if (!helperConnId) await ensurePeerKnown('helper');
        if (helperConnId) {
          console.log(`adapter HELLO: notifying helper ${helperConnId} of adapter connId`);
          const st = await wsSend(helperConnId, encodeControl(MSG_PEER_CONN, encodePeerConn(adapterConnId, token)), token);
          if (st >= 400) {
            console.log(`adapter HELLO: helper ${helperConnId} is stale (status=${st}), clearing`);
            helperConnId = null;
          }
        }
        console.log(`adapter authenticated connId=${adapterConnId} helperConnId=${helperConnId || 'null'} tokenLen=${token.length}`);
        return binaryResp(encodeControl(MSG_HELLO_OK, encodeHelloOK(adapterConnId, helperConnId, token)));
      }
      if (type === MSG_PING) {
        // Re-learn adapter connId from this message
        if (adapterConnId !== connId) {
          console.log(`adapter PING: re-learned adapterConnId=${connId} (was ${adapterConnId || 'null'})`);
          adapterConnId = connId;
        }
        if (!helperConnId) await ensurePeerKnown('helper');
        // If we now know both sides, notify helper of adapter (covers cross-instance state loss)
        if (helperConnId) {
          console.log(`adapter PING: cross-notifying helper ${helperConnId} of adapter connId`);
          const st = await wsSend(helperConnId, encodeControl(MSG_PEER_CONN, encodePeerConn(adapterConnId, token)), token);
          if (st >= 400) {
            console.log(`adapter PING: helper ${helperConnId} is stale (status=${st}), clearing`);
            helperConnId = null;
          }
        }
        console.log(`adapter PING -> PONG tokenLen=${token.length}`);
        return binaryResp(encodeControl(MSG_PONG, encodePong(token)));
      }
      if (type === MSG_SYNC) {
        if (!helperConnId) await ensurePeerKnown('helper');
        if (helperConnId) {
          console.log(`adapter SYNC -> PEER_CONN helperConnId=${helperConnId}`);
          return binaryResp(encodeControl(MSG_PEER_CONN, encodePeerConn(helperConnId, token)));
        }
        console.log('adapter SYNC -> PEER_GONE (no helper)');
        return binaryResp(encodeControl(MSG_PEER_GONE));
      }
      console.warn(`adapter MESSAGE: unhandled type=${msgName(type)}`);
      return { statusCode: 200 };
    }
    if (ev === 'DISCONNECT') {
      const wasKnown = adapterConnId === connId;
      if (wasKnown) adapterConnId = null;
      console.log(`adapter DISCONNECT connId=${connId} wasKnown=${wasKnown}`);
      if (helperConnId) {
        console.log(`adapter DISCONNECT: notifying helper ${helperConnId} of PEER_GONE`);
        await wsSend(helperConnId, encodeControl(MSG_PEER_GONE), token);
      }
      return { statusCode: 200 };
    }
    console.warn(`adapter: unknown event type=${ev}`);
    return { statusCode: 200 };
  }

  // --- HELPER ---
  if (route === 'helper') {
    if (ev === 'CONNECT') {
      helperConnId = connId;
      console.log(`helper CONNECT connId=${connId}`);
      return { statusCode: 200 };
    }
    if (ev === 'MESSAGE') {
      if (!helperConnId) { helperConnId = connId; console.log(`helper connId recovered from message: ${connId}`); }

      const buf = event.isBase64Encoded ? Buffer.from(event.body, 'base64') : Buffer.from(event.body || '');
      if (buf.length < 9) { console.warn(`helper MESSAGE too short len=${buf.length}`); return { statusCode: 200 }; }
      const type = buf[0];
      const streamId = buf.readUInt32BE(1);
      const seqId = buf.readUInt32BE(5);
      console.log(`helper MESSAGE type=${msgName(type)} streamId=${streamId} seq=${seqId} len=${buf.length}`);

      // QUIC packet (type 0x30) or batch (0x31): relay to adapter as-is
      if (type === MSG_QUIC || type === MSG_QUIC_BATCH) {
        if (!adapterConnId) await ensurePeerKnown('adapter');
        if (adapterConnId) {
          console.log(`relay ${msgName(type)} -> adapter ${adapterConnId} bytes=${buf.length}`);
          const st = await wsSend(adapterConnId, buf, token);
          if (st >= 400) {
            console.error(`relay ${msgName(type)} FAILED status=${st}, clearing stale adapterConnId=${adapterConnId}`);
            adapterConnId = null;
          }
        } else {
          console.warn(`relay DROP ${msgName(type)}: no adapter connected`);
        }
        return { statusCode: 200 };
      }

      if (type === MSG_HELLO) {
        const ver = buf[9];
        const tok = buf.subarray(10).toString('utf-8');
        if (ver !== 1 || tok !== AUTH_TOKEN) {
          console.error(`helper HELLO auth failed ver=${ver} tokenMatch=${tok === AUTH_TOKEN}`);
          return binaryResp(encodeControl(MSG_HELLO_ERR, Buffer.from('auth failed')));
        }
        if (!adapterConnId) await ensurePeerKnown('adapter');
        if (adapterConnId) {
          console.log(`helper HELLO: notifying adapter ${adapterConnId} of helper connId`);
          const st = await wsSend(adapterConnId, encodeControl(MSG_PEER_CONN, encodePeerConn(helperConnId, token)), token);
          if (st >= 400) {
            console.log(`helper HELLO: adapter ${adapterConnId} is stale (status=${st}), clearing`);
            adapterConnId = null;
          }
        }
        console.log(`helper authenticated connId=${helperConnId} adapterConnId=${adapterConnId || 'null'} tokenLen=${token.length}`);
        return binaryResp(encodeControl(MSG_HELLO_OK, encodeHelloOK(helperConnId, adapterConnId, token)));
      }
      if (type === MSG_PING) {
        // Re-learn helper connId from this message
        if (helperConnId !== connId) {
          console.log(`helper PING: re-learned helperConnId=${connId} (was ${helperConnId || 'null'})`);
          helperConnId = connId;
        }
        if (!adapterConnId) await ensurePeerKnown('adapter');
        // If we now know both sides, notify adapter of helper (covers cross-instance state loss)
        if (adapterConnId) {
          console.log(`helper PING: cross-notifying adapter ${adapterConnId} of helper connId`);
          const st = await wsSend(adapterConnId, encodeControl(MSG_PEER_CONN, encodePeerConn(helperConnId, token)), token);
          if (st >= 400) {
            console.log(`helper PING: adapter ${adapterConnId} is stale (status=${st}), clearing`);
            adapterConnId = null;
          }
        }
        console.log(`helper PING -> PONG tokenLen=${token.length}`);
        return binaryResp(encodeControl(MSG_PONG, encodePong(token)));
      }
      if (type === MSG_SYNC) {
        if (!adapterConnId) await ensurePeerKnown('adapter');
        if (adapterConnId) {
          console.log(`helper SYNC -> PEER_CONN adapterConnId=${adapterConnId}`);
          return binaryResp(encodeControl(MSG_PEER_CONN, encodePeerConn(adapterConnId, token)));
        }
        console.log('helper SYNC -> PEER_GONE (no adapter)');
        return binaryResp(encodeControl(MSG_PEER_GONE));
      }
      console.warn(`helper MESSAGE: unhandled type=${msgName(type)}`);
      return { statusCode: 200 };
    }
    if (ev === 'DISCONNECT') {
      const wasKnown = helperConnId === connId;
      if (wasKnown) helperConnId = null;
      console.log(`helper DISCONNECT connId=${connId} wasKnown=${wasKnown}`);
      if (adapterConnId) {
        console.log(`helper DISCONNECT: notifying adapter ${adapterConnId} of PEER_GONE`);
        await wsSend(adapterConnId, encodeControl(MSG_PEER_GONE), token);
      }
      return { statusCode: 200 };
    }
    console.warn(`helper: unknown event type=${ev}`);
    return { statusCode: 200 };
  }

  console.warn(`unknown route=${route} event=${ev} connId=${connId}`);
  return { statusCode: 200 };
}
