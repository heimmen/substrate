#!/usr/bin/env python3
"""Temporary CI/CD page for the mf-pi demo.

Serves a one-button page on 127.0.0.1 and, when that button is pressed, runs
deploy.sh on **this host** (it needs the repo, ko, docker and kubectl, so it
cannot run inside the admin pod) while streaming the script's output back to the
browser.

It is reachable at http://<gateway>/_cicd_ because mfpi-nginx proxies there and
enforces the same Basic Auth as /usermanagement/ — see nginx.conf. The server
itself listens on the loopback interface only: nginx is the only intended
client, and it is the component that authenticates the caller.

THIS IS A DEMO TOOL. Anyone who can pass the admin Basic Auth can run a deploy
on this machine. Do not expose it beyond the demo host.
"""

import json
import os
import subprocess
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse, parse_qs

PORT = int(os.environ.get("CICD_PORT", "58690"))
DEMO_DIR = os.path.dirname(os.path.abspath(__file__))
# CICD_SCRIPT exists so the page/mechanism can be exercised with a stub instead
# of a real deploy (tests, demos). Production use leaves it unset.
SCRIPT = os.environ.get("CICD_SCRIPT", "./deploy.sh")

# One run at a time. `lines` holds the current (or most recent) run's output so a
# browser that reloads mid-deploy can replay it from the start.
COND = threading.Condition()
STATE = {
    "id": 0,
    "lines": [],
    "running": False,
    "rc": None,
    "started": None,
    "ended": None,
}


def publish(line):
    with COND:
        STATE["lines"].append(line)
        COND.notify_all()


def snapshot(since):
    with COND:
        return {
            "id": STATE["id"],
            "lines": STATE["lines"][since:],
            "next": len(STATE["lines"]),
            "running": STATE["running"],
            "rc": STATE["rc"],
            "started": STATE["started"],
            "ended": STATE["ended"],
            "count": len(STATE["lines"]),
        }


def run_deploy():
    started = STATE["started"]
    publish("$ " + SCRIPT + "   (cwd=%s)" % DEMO_DIR)
    publish("started at " + time.strftime("%Y-%m-%d %H:%M:%S"))
    rc = None
    try:
        env = os.environ.copy()
        env.setdefault("TERM", "dumb")
        proc = subprocess.Popen(
            ["/bin/bash", SCRIPT],
            cwd=DEMO_DIR,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
            bufsize=1,
            env=env,
            start_new_session=True,
        )
        for line in proc.stdout:
            publish(line.rstrip("\n"))
        rc = proc.wait()
    except Exception as exc:  # noqa: BLE001 - surface any failure in the log
        publish("failed to run %s: %s" % (SCRIPT, exc))
        rc = -1
    elapsed = time.time() - (started or time.time())
    publish(
        "%s rc=%d elapsed=%.0fs"
        % ("✔ deploy finished" if rc == 0 else "✖ deploy failed", rc, elapsed)
    )
    with COND:
        STATE["running"] = False
        STATE["rc"] = rc
        STATE["ended"] = time.time()
        COND.notify_all()


def start_deploy():
    with COND:
        if STATE["running"]:
            return False
        STATE["id"] += 1
        STATE["lines"] = []
        STATE["running"] = True
        STATE["rc"] = None
        STATE["started"] = time.time()
        STATE["ended"] = None
    threading.Thread(target=run_deploy, daemon=True).start()
    return True


class Handler(BaseHTTPRequestHandler):
    server_version = "mfpi-cicd/1"

    def log_message(self, fmt, *args):  # keep the console quiet
        sys.stderr.write("%s - %s\n" % (self.address_string(), fmt % args))

    def _send(self, code, body, ctype="application/json; charset=utf-8"):
        raw = body.encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(raw)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(raw)

    def _json(self, code, obj):
        self._send(code, json.dumps(obj))

    def do_GET(self):
        parsed = urlparse(self.path)
        if parsed.path in ("/", ""):
            self._send(200, PAGE, "text/html; charset=utf-8")
            return
        if parsed.path == "/state":
            since = int(parse_qs(parsed.query).get("since", ["0"])[0] or 0)
            self._json(200, snapshot(max(0, since)))
            return
        self._json(404, {"error": "not found"})

    def do_POST(self):
        parsed = urlparse(self.path)
        if parsed.path != "/deploy":
            self._json(404, {"error": "not found"})
            return
        if not start_deploy():
            self._json(409, {"error": "a deploy is already running"})
            return
        self._json(202, {"ok": True})


PAGE = """<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8" />
<meta name="viewport" content="width=device-width, initial-scale=1" />
<title>秒方 CI/CD（临时）</title>
<style>
  :root {
    --bg: #FAFAFA; --rail: #F3F1EE; --card: #FFFFFF; --border: #E7E3DE;
    --hover: #F1EEEA; --text: #303234; --muted: #6B6660;
    --accent: #CC3733; --on-accent: #FFFFFF;
    --ok: #2F6B3C; --ok-bg: #EFF6F0; --warn: #8A6A1F; --warn-bg: #FDF6E3;
    --danger: #8C1D18; --danger-bg: #FDF2F0;
  }
  * { box-sizing: border-box; }
  body {
    margin: 0; background: var(--bg); color: var(--text);
    font: 14px/1.55 -apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC",
          "Hiragino Sans GB", "Microsoft YaHei", sans-serif;
  }
  header {
    display: flex; align-items: center; gap: 12px;
    padding: 16px 24px; background: var(--rail);
    border-bottom: 1px solid var(--border);
  }
  header h1 { margin: 0; font-size: 16px; font-weight: 650; letter-spacing: .2px; }
  header .sub { color: var(--muted); font-size: 12px; }
  header .tag {
    margin-left: auto; padding: 3px 12px; border-radius: 999px;
    font-size: 12px; background: var(--warn-bg); color: var(--warn);
    border: 1px solid #EFE3C2;
  }
  main { max-width: 1080px; margin: 0 auto; padding: 24px; }
  .card {
    background: var(--card); border: 1px solid var(--border);
    border-radius: 12px; padding: 20px 22px; margin-bottom: 18px;
  }
  .status { display: flex; align-items: center; gap: 12px; flex-wrap: wrap; }
  .dot { width: 9px; height: 9px; border-radius: 50%; background: var(--muted); flex: none; }
  .dot.run { background: var(--accent); animation: pulse 1.2s ease-in-out infinite; }
  .dot.ok { background: var(--ok); }
  .dot.bad { background: var(--danger); }
  @keyframes pulse { 0%,100% { opacity: 1 } 50% { opacity: .35 } }
  .label { font-weight: 600; }
  .muted { color: var(--muted); font-size: 12px; }
  button {
    appearance: none; cursor: pointer; font: inherit;
    padding: 10px 18px; border-radius: 8px; border: 1px solid transparent;
    background: var(--accent); color: var(--on-accent); font-weight: 600;
  }
  button:hover { background: #B62F2B; }
  button:disabled { background: var(--hover); color: var(--muted); cursor: not-allowed; }
  .row { display: flex; align-items: center; gap: 14px; margin-top: 16px; flex-wrap: wrap; }
  .hint {
    margin-top: 14px; padding: 10px 14px; border-radius: 8px;
    background: var(--warn-bg); color: var(--warn); border-left: 3px solid #E0C97F;
    font-size: 12px; line-height: 1.6;
  }
  pre {
    margin: 0; padding: 16px; border-radius: 8px; background: #1E1F21; color: #E6E3DE;
    font: 12px/1.55 ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
    min-height: 320px; max-height: 62vh; overflow: auto; white-space: pre-wrap;
    word-break: break-word;
  }
  .logcard { padding: 16px; }
  .loghead {
    display: flex; align-items: center; gap: 10px; margin-bottom: 10px;
  }
  .loghead .muted { margin-left: auto; }
</style>
</head>
<body>
<header>
  <h1>秒方 CI/CD</h1>
  <span class="sub">主机 <code>demos/mf-pi</code> · 运行 <code>./deploy.sh</code></span>
  <span class="tag">临时页面</span>
</header>
<main>
  <section class="card">
    <div class="status">
      <span class="dot" id="dot"></span>
      <span class="label" id="state-label">正在读取状态…</span>
      <span class="muted" id="state-meta"></span>
    </div>
    <div class="row">
      <button id="deploy">Deploy latest code</button>
      <span class="muted">点击后在部署机（本机）执行 <code>./deploy.sh</code>，输出实时显示在下方。</span>
    </div>
    <div class="hint">
      临时工具：任何人通过 <code>/usermanagement/</code> 的管理员口令都能触发一次部署。
      部署会重新构建 admin 镜像并滚动 nginx / admin，期间管理页可能短暂 502。
    </div>
  </section>

  <section class="card logcard">
    <div class="loghead">
      <strong>输出</strong>
      <span class="muted" id="log-meta">尚无输出</span>
    </div>
    <pre id="log">等待开始…</pre>
  </section>
</main>

<script>
(function () {
  var BASE = "/_cicd_";
  var btn = document.getElementById("deploy");
  var dot = document.getElementById("dot");
  var label = document.getElementById("state-label");
  var meta = document.getElementById("state-meta");
  var log = document.getElementById("log");
  var logMeta = document.getElementById("log-meta");
  var since = 0;
  var timer = null;
  var startedAt = null;

  function esc(s) {
    return String(s).replace(/[&<>]/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;" }[c];
    });
  }

  function paint(state, delta) {
    if (delta && delta.lines && delta.lines.length) {
      var first = since === 0;
      if (first) { log.textContent = ""; }
      log.textContent += delta.lines.map(esc).join("\\n") + "\\n";
      log.scrollTop = log.scrollHeight;
      since = delta.next;
      logMeta.textContent = since + " 行";
    }
    var running = state.running;
    btn.disabled = running;
    if (running) {
      dot.className = "dot run";
      label.textContent = "正在部署…";
      if (startedAt) {
        meta.textContent = "已运行 " + Math.max(0, Math.round((Date.now() - startedAt) / 1000)) + " 秒";
      }
    } else if (state.rc === 0) {
      dot.className = "dot ok";
      label.textContent = "上一次部署成功";
      meta.textContent = state.ended ? new Date(state.ended * 1000).toLocaleString() : "";
    } else if (state.rc !== null && state.rc !== undefined) {
      dot.className = "dot bad";
      label.textContent = "上一次部署失败（rc=" + state.rc + "）";
      meta.textContent = state.ended ? new Date(state.ended * 1000).toLocaleString() : "";
    } else {
      dot.className = "dot";
      label.textContent = "空闲";
      meta.textContent = "点按上方按钮开始";
    }
  }

  function poll() {
    fetch(BASE + "/state?since=" + since, { cache: "no-store" })
      .then(function (r) { return r.json(); })
      .then(function (d) {
        if (d.id !== window.__runId && since > 0) {
          // A new run started elsewhere: replay it from the top.
          since = 0;
          window.__runId = d.id;
          return poll();
        }
        window.__runId = d.id;
        if (d.started) { startedAt = d.started * 1000; }
        paint(d, d);
        if (d.running) { timer = window.setTimeout(poll, 800); }
        else { timer = null; }
      })
      .catch(function () { timer = window.setTimeout(poll, 2000); });
  }

  btn.addEventListener("click", function () {
    btn.disabled = true;
    if (timer) { window.clearTimeout(timer); timer = null; }
    since = 0;
    log.textContent = "";
    fetch(BASE + "/deploy", { method: "POST" })
      .then(function (r) {
        if (r.status === 409) {
          log.textContent = "已有部署正在运行，请等待它结束。\\n";
        }
        return r.json().catch(function () { return {}; });
      })
      .then(function () { poll(); })
      .catch(function (e) {
        log.textContent = "无法启动部署：" + e + "\\n";
        btn.disabled = false;
      });
  });

  // Only refresh the elapsed counter while a run is in flight: re-painting the
  // whole status here would erase the final "succeeded"/"failed" result.
  window.setInterval(function () {
    if (!btn.disabled || !startedAt) { return; }
    meta.textContent = "已运行 " + Math.max(0, Math.round((Date.now() - startedAt) / 1000)) + " 秒";
  }, 1000);

  poll();
})();
</script>
</body>
</html>
"""


def main():
    if not os.path.exists(os.path.join(DEMO_DIR, "deploy.sh")):
        sys.exit("deploy.sh not found next to this script (%s)" % DEMO_DIR)
    srv = ThreadingHTTPServer(("127.0.0.1", PORT), Handler)
    srv.daemon_threads = True
    print("mfpi-cicd listening on http://127.0.0.1:%d/ (execute %s in %s)"
          % (PORT, SCRIPT, DEMO_DIR), flush=True)
    srv.serve_forever()


if __name__ == "__main__":
    main()
