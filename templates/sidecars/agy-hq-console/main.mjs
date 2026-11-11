#!/usr/bin/env node
import http from 'node:http';
import { spawn } from 'node:child_process';
import { existsSync, readFileSync } from 'node:fs';
import { homedir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = dirname(fileURLToPath(import.meta.url));
const homeDir = process.env.HOME || homedir();
const TARGET_PORT = 4242;
const TARGET_HOST = '127.0.0.1';

function resolveHQBin() {
  if (process.env.HQ_BIN_PATH && existsSync(process.env.HQ_BIN_PATH)) {
    return process.env.HQ_BIN_PATH;
  }
  let dir = __dirname;
  for (let i = 0; i < 8; i++) {
    const candidate = join(dir, 'bin', 'hq');
    if (existsSync(candidate)) return candidate;
    const parent = dirname(dir);
    if (parent === dir) break;
    dir = parent;
  }
  return join(process.cwd(), 'bin', 'hq');
}
const HQ_BIN = resolveHQBin();

// Preload JS location or fallback
let preloadJS = '';
const possiblePreloads = [
  join(homeDir, '.gemini/config/node_modules/sidecar_sdk/preload.js'),
  join(homeDir, '.gemini/antigravity/lib/node_modules/sidecar_sdk/preload.js'),
  join(__dirname, 'preload.js'),
];
for (const p of possiblePreloads) {
  if (existsSync(p)) {
    try {
      preloadJS = readFileSync(p, 'utf-8');
      break;
    } catch {}
  }
}
if (!preloadJS) {
  preloadJS = `(function() {
  const params = new URLSearchParams(window.location.search);
  const token = params.get('token') || '';
  window.addEventListener('message', (event) => {
    if (event.data?.type === 'theme-change' && event.data.payload?.cssVariables) {
      for (const [k, v] of Object.entries(event.data.payload.cssVariables)) {
        document.documentElement.style.setProperty(k, v);
      }
    }
  });
  window.sidecar = {
    token: token,
    fetch: (url, opts = {}) => {
      opts.headers = opts.headers || {};
      if (token && !opts.headers['x-sidecar-token']) opts.headers['x-sidecar-token'] = token;
      return fetch(url, opts);
    }
  };
})();`;
}

// Auto-start HQ daemon if not responding
function ensureDaemon() {
  const req = http.request({
    hostname: TARGET_HOST,
    port: TARGET_PORT,
    path: '/healthz',
    method: 'GET',
    timeout: 1000,
  }, (res) => {
    // Daemon is alive
  });
  req.on('error', () => {
    if (existsSync(HQ_BIN)) {
      try {
        const p = spawn(HQ_BIN, ['daemon', `--addr=${TARGET_HOST}:${TARGET_PORT}`], {
          detached: true,
          stdio: 'ignore',
        });
        p.unref();
        console.log('[sidecar] spawned agy-hq daemon on port', TARGET_PORT);
      } catch (err) {
        console.error('[sidecar] error spawning hq daemon:', err);
      }
    }
  });
  req.end();
}

ensureDaemon();
setInterval(ensureDaemon, 10000);

const listenPort = process.env.ANTIGRAVITY_SIDECAR_WEB_PORT
  ? parseInt(process.env.ANTIGRAVITY_SIDECAR_WEB_PORT, 10)
  : (process.env.PORT ? parseInt(process.env.PORT, 10) : TARGET_PORT);

const token = process.env.ANTIGRAVITY_SIDECAR_UI_TOKEN || '';

function proxy(req, res, targetPath) {
  const path = targetPath || req.url;
  const opts = {
    hostname: TARGET_HOST,
    port: TARGET_PORT,
    path: path,
    method: req.method,
    headers: { ...req.headers, host: `${TARGET_HOST}:${TARGET_PORT}` },
  };

  const pReq = http.request(opts, (pRes) => {
    res.writeHead(pRes.statusCode, pRes.headers);
    pRes.pipe(res, { end: true });
  });

  pReq.on('error', (err) => {
    res.writeHead(502, { 'Content-Type': 'text/html; charset=utf-8' });
    res.end(`<!DOCTYPE html><html><body style="background:#0b0f19;color:#e6edf3;font-family:sans-serif;padding:24px;">
      <h2>Connecting to agy-hq daemon...</h2>
      <p style="color:#8b949e">Daemon is launching on 127.0.0.1:${TARGET_PORT}. Connecting...</p>
      <script>setTimeout(() => location.reload(), 1500);</script>
    </body></html>`);
  });

  req.pipe(pReq, { end: true });
}

if (process.env.ANTIGRAVITY_SIDECAR_WEB_PORT && listenPort !== TARGET_PORT) {
  const server = http.createServer((req, res) => {
    const url = new URL(req.url, `http://localhost:${listenPort}`);
    if (url.pathname === '/preload.js') {
      res.writeHead(200, { 'Content-Type': 'application/javascript; charset=utf-8' });
      res.end(preloadJS);
      return;
    }
    if (token && req.method !== 'GET') {
      const provided = req.headers['x-sidecar-token'] || url.searchParams.get('token');
      if (!provided || provided !== token) {
        res.writeHead(401, { 'Content-Type': 'application/json' });
        res.end(JSON.stringify({ error: 'unauthorized' }));
        return;
      }
    }
    proxy(req, res);
  });

  server.listen(listenPort, '127.0.0.1', () => {
    console.log(`[sidecar] agy-hq-console listening on 127.0.0.1:${listenPort} (proxying to :${TARGET_PORT})`);
  });
} else {
  console.log(`[sidecar] agy-hq-console initialized (daemon target 127.0.0.1:${TARGET_PORT})`);
}
