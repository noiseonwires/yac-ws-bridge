// Bridge to Freedom - Yandex Cloud Function (v3: hybrid)
// Bridge->Adapter: upstream WS | Adapter->Client: direct WS API
//
// Env: AUTH_TOKEN, ADAPTER_URL (optional POST fallback),
//      FORWARD_HTTP ("true"/"1" to proxy plain HTTP to the target via the adapter)

const https = require('https');
const http = require('http');

const AUTH_TOKEN = process.env.AUTH_TOKEN;
const WAKEUP_URL = process.env.ADAPTER_URL || null;
const FORWARD_HTTP = ['true','1'].includes((process.env.FORWARD_HTTP||'').toLowerCase());

const httpsAgent = new https.Agent({ keepAlive: true });
const httpAgent = new http.Agent({ keepAlive: true });

// Build a full URL by appending a suffix to ADAPTER_URL's existing path.
function adapterUrl(suffix) {
  const u = new URL(WAKEUP_URL);
  u.pathname = u.pathname.replace(/\/+$/, '') + '/' + suffix;
  return u.toString();
}

// --- State ---
let upstreamConnId = null;
let _fetchPromise = null;

// --- Helpers ---
function getHeader(h, n) {
  if (!h) return '';
  // Try exact match first, then case-insensitive scan
  if (h[n]) return h[n];
  const lower = n.toLowerCase();
  for (const k of Object.keys(h)) {
    if (k.toLowerCase() === lower) return h[k];
  }
  return '';
}

function httpPost(url, headers, body) {
  return new Promise((resolve, reject) => {
    const mod = url.startsWith('https') ? https : http;
    const agent = url.startsWith('https') ? httpsAgent : httpAgent;
    const p = new URL(url);
    const req = mod.request({
      hostname: p.hostname, port: p.port || (p.protocol === 'https:' ? 443 : 80),
      path: p.pathname + p.search, method: 'POST', agent,
      headers: { ...headers, 'Content-Length': Buffer.byteLength(body) },
    }, res => { let d=''; res.on('data',c=>d+=c); res.on('end',()=>resolve({status:res.statusCode,body:d})); });
    req.on('error', reject);
    req.write(body);
    req.end();
  });
}

function httpGet(url, headers) {
  return new Promise((resolve, reject) => {
    const mod = url.startsWith('https') ? https : http;
    const agent = url.startsWith('https') ? httpsAgent : httpAgent;
    const p = new URL(url);
    const req = mod.request({
      hostname: p.hostname, port: p.port || (p.protocol === 'https:' ? 443 : 80),
      path: p.pathname + p.search, method: 'GET', agent,
      headers: headers || {},
    }, res => { let d=''; res.on('data',c=>d+=c); res.on('end',()=>resolve({status:res.statusCode,body:d})); });
    req.on('error', reject);
    req.end();
  });
}

async function fetchUpstreamConnId() {
  if (!WAKEUP_URL) return;
  try {
    const url = adapterUrl('upstream-id');
    console.log('fetching upstream connId from adapter...');
    const r = await httpGet(url, { 'Authorization': 'Bearer ' + AUTH_TOKEN });
    if (r.status === 200 && r.body) {
      upstreamConnId = r.body;
      console.log('fetched upstream connId from adapter:', upstreamConnId);
    } else {
      console.log('adapter returned no upstream connId, status:', r.status);
    }
  } catch(e) { console.error('fetchUpstreamConnId err:', e.message || e); }
}

// Fetch upstream connId at module init (YC may pre-spawn instances)
_fetchPromise = fetchUpstreamConnId();

// WS management API - send to a specific connection
async function wsSend(connId, data, type, token) {
  const b64 = Buffer.from(data).toString('base64');
  const body = JSON.stringify({ data: b64, type: type });
  try {
    const r = await httpPost(
      `https://apigateway-connections.api.cloud.yandex.net/apigateways/websocket/v1/connections/${encodeURIComponent(connId)}:send`,
      { 'Content-Type': 'application/json', 'Authorization': 'Bearer ' + token }, body
    );
    if (r.status >= 300) console.error('wsSend fail:', r.status, connId);
    return r.status;
  } catch(e) { console.error('wsSend err:', e, connId); return 500; }
}

// --- Protocol (string client IDs, no batching in v3) ---
function encode(clientId, type, flags, payload) {
  const cid = Buffer.from(clientId, 'utf-8');
  const h = Buffer.alloc(2 + cid.length + 2);
  h.writeUInt16BE(cid.length, 0);
  cid.copy(h, 2);
  h[2 + cid.length] = type;
  h[2 + cid.length + 1] = flags;
  return payload ? Buffer.concat([h, payload]) : h;
}

function decode(buf) {
  const cidLen = buf.readUInt16BE(0);
  const off = 2 + cidLen;
  return { clientId: buf.subarray(2, 2+cidLen).toString('utf-8'), type: buf[off], flags: buf[off+1], payload: buf.subarray(off+2) };
}

// CLIENT_CONNECTED payload: [2B pathLen][path][2B subLen][sub][2B tokenLen][token]
function encodeClientConnected(path, subprotocols, iamToken) {
  const p = Buffer.from(path, 'utf-8');
  const s = Buffer.from(subprotocols, 'utf-8');
  const t = Buffer.from(iamToken, 'utf-8');
  const buf = Buffer.alloc(2 + p.length + 2 + s.length + 2 + t.length);
  let off = 0;
  buf.writeUInt16BE(p.length, off); off += 2; p.copy(buf, off); off += p.length;
  buf.writeUInt16BE(s.length, off); off += 2; s.copy(buf, off); off += s.length;
  buf.writeUInt16BE(t.length, off); off += 2; t.copy(buf, off);
  return buf;
}

const MSG_HELLO=0x01, MSG_HELLO_OK=0x02, MSG_HELLO_ERR=0x03;
const MSG_CLIENT_CONNECTED=0x10, MSG_CLIENT_DISCONNECTED=0x11;
const MSG_DATA_C2T=0x20, MSG_PING=0xF0, MSG_PONG=0xF1;
const FLAG_TEXT=0x01;

// Send frame to adapter via upstream WS, POST fallback if unavailable.
async function sendToAdapter(frame, iamToken) {
  if (upstreamConnId) {
    const st = await wsSend(upstreamConnId, frame, 'BINARY', iamToken);
    if (st < 400) return;
    console.error('upstream WS send failed, status:', st, 'connId:', upstreamConnId, '- falling back to POST');
    upstreamConnId = null;
  }
  // POST fallback — also triggers adapter to connect upstream
  if (!WAKEUP_URL) return;
  console.log('sending frame via POST fallback');
  try {
    const r = await httpPost(WAKEUP_URL, {
      'Content-Type': 'application/octet-stream',
      'Authorization': 'Bearer ' + AUTH_TOKEN,
    }, Buffer.from(frame));
    if (r.status === 200 && r.body) {
      upstreamConnId = r.body;
      console.log('recovered upstream connId from POST response:', upstreamConnId);
    }
  } catch(e) { console.error('wakeup POST err:', e.message || e); }
}

// --- HTTP forwarding ---
async function forwardHTTP(event) {
  if (!WAKEUP_URL) return { statusCode: 502, body: 'no adapter URL configured' };
  const url = adapterUrl('proxy');

  const proxyReq = {
    method: event.httpMethod || 'GET',
    path: event.path || '/',
    queryString: event.queryStringParameters || {},
    headers: event.headers || {},
    body: event.body || '',
    isBase64Encoded: event.isBase64Encoded || false,
  };

  try {
    const r = await httpPost(url, {
      'Content-Type': 'application/json',
      'Authorization': 'Bearer ' + AUTH_TOKEN,
    }, JSON.stringify(proxyReq));
    const resp = JSON.parse(r.body);
    return resp;
  } catch(e) {
    console.error('forwardHTTP err:', e.message || e);
    return { statusCode: 502, body: 'proxy error' };
  }
}

// --- Handler ---
module.exports.handler = async function(event, context) {
  try { return await handle(event, context); }
  catch(e) { console.error('ERROR:', e.stack||e); return {statusCode:200}; }
};

async function handle(event, context) {
  const rc = event.requestContext || {};
  const connId = rc.connectionId;
  const ev = rc.eventType;
  const token = context.token?.access_token || '';
  const route = rc.apiGateway?.operationContext?.route || 'client';

  // --- HTTP forwarding (non-WebSocket request) ---
  if (!ev && FORWARD_HTTP) {
    return await forwardHTTP(event);
  }

  // --- UPSTREAM (adapter) ---
  if (route === 'upstream') {
    if (ev === 'CONNECT') {
      upstreamConnId = connId;
      console.log('upstream connected:', connId);
      return { statusCode: 200 };
    }
    if (ev === 'MESSAGE') {
      if (!upstreamConnId) { upstreamConnId = connId; console.log('upstream recovered:', connId); }
      const buf = event.isBase64Encoded ? Buffer.from(event.body,'base64') : Buffer.from(event.body||'');
      const f = decode(buf);
      if (f.type === MSG_HELLO) {
        const ver = f.payload[0];
        const tok = f.payload.subarray(1).toString('utf-8');
        if (ver !== 1 || tok !== AUTH_TOKEN) {
          return binaryResp(encode('', MSG_HELLO_ERR, 0, Buffer.from('auth failed')));
        }
        console.log('adapter authenticated, upstream connId:', upstreamConnId);
        return binaryResp(encode('', MSG_HELLO_OK, 0, Buffer.from(upstreamConnId || '')));
      }
      if (f.type === MSG_PING) return binaryResp(encode('', MSG_PONG, 0));
      return { statusCode: 200 };
    }
    if (ev === 'DISCONNECT') {
      if (upstreamConnId === connId) upstreamConnId = null;
      console.log('upstream disconnected');
      return { statusCode: 200 };
    }
    return { statusCode: 200 };
  }

  // --- CLIENT ---
  if (_fetchPromise) {
    await _fetchPromise;
    _fetchPromise = null;
  }

  if (ev === 'CONNECT') {
    const path = event.path || '/';
    const sub = getHeader(event.headers, 'Sec-WebSocket-Protocol');
    console.log('client CONNECT:', connId, 'path:', path, 'subproto:', sub ? sub.substring(0,50)+'...' : '(none)', 'allHeaders:', JSON.stringify(Object.keys(event.headers || {})));
    const payload = encodeClientConnected(path, sub, token);
    await sendToAdapter(encode(connId, MSG_CLIENT_CONNECTED, 0, payload), token);
    const headers = {};
    if (sub) headers['Sec-WebSocket-Protocol'] = sub;
    console.log('client CONNECT response headers:', JSON.stringify(headers).substring(0, 200));
    return { statusCode: 200, headers };
  }

  if (ev === 'MESSAGE') {
    const buf = event.isBase64Encoded ? Buffer.from(event.body,'base64') : Buffer.from(event.body||'');
    const ct = getHeader(event.headers, 'Content-Type');
    const isText = ct.startsWith('application/json') || ct.startsWith('text/');
    console.log('client MSG:', connId, 'len:', buf.length, 'ct:', ct, 'isText:', isText, 'isBase64:', event.isBase64Encoded);
    await sendToAdapter(encode(connId, MSG_DATA_C2T, isText ? FLAG_TEXT : 0, buf), token);
    return { statusCode: 200 };
  }

  if (ev === 'DISCONNECT') {
    await sendToAdapter(encode(connId, MSG_CLIENT_DISCONNECTED, 0), token);
    return { statusCode: 200 };
  }

  return { statusCode: 200 };
}

function binaryResp(buf) {
  return { statusCode:200, headers:{'Content-Type':'application/octet-stream'}, body:buf.toString('base64'), isBase64Encoded:true };
}