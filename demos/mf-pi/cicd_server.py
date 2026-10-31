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

import errno
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


# REPO_DIR is the substrate checkout that contains this demo (demos/mf-pi -> ../..).
REPO_DIR = os.path.dirname(os.path.dirname(DEMO_DIR))

# WORKLOAD_DIR is the miaofang-agent checkout that deploy.sh builds into the
# workload image (MFPI_WORKLOAD_SOURCE). Showing its commits lets the page tell
# whether the about-to-be-deployed image already contains a given commit.
WORKLOAD_DIR = os.environ.get("MFPI_WORKLOAD_SOURCE",
                              "/home/liuchong/git/miaofang-agent")
# How many recent commits to surface on the page.
COMMIT_COUNT = int(os.environ.get("CICD_COMMIT_COUNT", "8"))


def git_in(repo, *args):
    return subprocess.run(["git", "-C", repo] + list(args),
                          capture_output=True, text=True)


def current_branch(repo):
    if git_in(repo, "rev-parse", "--is-inside-work-tree").returncode != 0:
        return None
    return git_in(repo, "rev-parse", "--abbrev-ref", "HEAD").stdout.strip() or "(detached)"


def git_commits(repo, n=COMMIT_COUNT):
    """Last n commits of repo as a list of dicts, or None if not a git checkout."""
    if git_in(repo, "rev-parse", "--is-inside-work-tree").returncode != 0:
        return None
    # Fields split by U+001F; the refs field (%d) may be empty.
    res = git_in(repo, "log", "-n", str(n),
                 "--format=%H%x1f%h%x1f%s%x1f%an%x1f%ar%x1f%ad%x1f%d")
    if res.returncode != 0:
        return None
    commits = []
    for line in res.stdout.splitlines():
        p = line.split("\x1f")
        if len(p) < 6:
            continue
        commits.append({
            "hash": p[0], "short": p[1], "subject": p[2],
            "author": p[3], "rel": p[4], "date": p[5],
            "refs": p[6] if len(p) > 6 else "",
        })
    return commits


def repo_status(repo):
    """Branch, HEAD commits, and how far local is behind its upstream.

    A best-effort `git fetch` refreshes the remote-tracking ref so `behind`
    reflects what a deploy would pull; fetch failures are reported, never fatal.
    """
    branch = current_branch(repo)
    if branch is None:
        return {"dir": repo, "branch": None, "commits": None,
                "behind": None, "error": "not a git checkout"}
    info = {"dir": repo, "branch": branch, "commits": git_commits(repo)}
    remote = os.environ.get("CICD_REPO_REMOTE", "origin")
    fetch = git_in(repo, "fetch", "--prune", remote)
    if fetch.returncode != 0:
        info["behind"] = None
        info["fetch_error"] = (fetch.stderr or fetch.stdout).strip().splitlines()[-1:]
        return info
    upstream = "%s/%s" % (remote, branch)
    if git_in(repo, "rev-parse", "--verify", upstream).returncode != 0:
        info["behind"] = None
        return info
    behind = git_in(repo, "rev-list", "--count", "HEAD..%s" % upstream)
    ahead = git_in(repo, "rev-list", "--count", "%s..HEAD" % upstream)
    info["behind"] = int(behind.stdout.strip() or 0) if behind.returncode == 0 else None
    info["ahead"] = int(ahead.stdout.strip() or 0) if ahead.returncode == 0 else None
    return info


def repos_commits():
    return {"substrate": repo_status(REPO_DIR),
            "workload": repo_status(WORKLOAD_DIR)}


def pull_repo():
    """Update the substrate checkout (admin/nginx/templates/deploy.sh) before deploy.

    deploy.sh only pulls the *workload* checkout (miaofang-agent); the script it
    runs, the admin sources and the manifests all come from this repository. Without
    this step "Deploy latest code" would run whatever happens to be on disk, which
    is the same trap the workload pull had. Best-effort on purpose: a diverged or
    dirty demo checkout must not block an operator's deploy, so we log and continue.
    Opt out with CICD_SKIP_REPO_PULL=1.
    """
    if os.environ.get("CICD_SKIP_REPO_PULL") == "1":
        publish("[cicd] CICD_SKIP_REPO_PULL=1: not updating %s" % REPO_DIR)
        return
    def git(*args):
        return subprocess.run(["git", "-C", REPO_DIR] + list(args),
                              capture_output=True, text=True)
    inside = git("rev-parse", "--is-inside-work-tree")
    if inside.returncode != 0:
        publish("[cicd] %s is not a git checkout; using it as-is" % REPO_DIR)
        return
    branch = git("rev-parse", "--abbrev-ref", "HEAD").stdout.strip() or "HEAD"
    remote = os.environ.get("CICD_REPO_REMOTE", "origin")
    publish("[cicd] updating %s (%s) from %s ..." % (REPO_DIR, branch, remote))
    fetch = git("fetch", "--prune", remote)
    if fetch.returncode != 0:
        publish("[cicd] WARNING: git fetch failed; deploying the checkout as-is:")
        for line in (fetch.stderr or fetch.stdout).strip().splitlines():
            publish("[cicd]   " + line)
        return
    merge = git("merge", "--ff-only", "%s/%s" % (remote, branch))
    if merge.returncode != 0:
        publish("[cicd] WARNING: %s cannot fast-forward to %s/%s; deploying as-is:"
                % (branch, remote, branch))
        for line in (merge.stderr or merge.stdout).strip().splitlines():
            publish("[cicd]   " + line)
        return
    head = git("rev-parse", "--short", "HEAD").stdout.strip()
    subject = git("log", "-1", "--format=%s").stdout.strip()
    publish("[cicd]   %s now at %s %s" % (REPO_DIR, head, subject))


def run_deploy():
    started = STATE["started"]
    publish("$ " + SCRIPT + "   (cwd=%s)" % DEMO_DIR)
    publish("started at " + time.strftime("%Y-%m-%d %H:%M:%S"))
    rc = None
    try:
        pull_repo()
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
            self._send(200, PAGE.replace("__CICD_SCRIPT__", SCRIPT), "text/html; charset=utf-8")
            return
        if parsed.path == "/state":
            since = int(parse_qs(parsed.query).get("since", ["0"])[0] or 0)
            self._json(200, snapshot(max(0, since)))
            return
        if parsed.path == "/commits":
            self._json(200, repos_commits())
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
  .repo { padding: 10px 0; border-bottom: 1px solid var(--border); }
  .repo:last-child { border-bottom: none; }
  .repo-head { margin-bottom: 6px; }
  .repo-name { font-weight: 600; }
  .behind { color: var(--warn); font-size: 12px; margin: 4px 0; }
  .uptodate { color: var(--ok); font-size: 12px; margin: 4px 0; }
  .commit-list { list-style: none; margin: 6px 0 0; padding: 0; }
  .commit-list li { padding: 3px 0; }
  .commit-list li.head { border-left: 2px solid var(--accent); padding-left: 8px; background: var(--warn-bg); }
  .commit-list .ch { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; color: var(--accent); }
</style>
</head>
<body>
<header>
  <h1>秒方 CI/CD</h1>
  <span class="sub">主机 <code>demos/mf-pi</code> · 运行 <code>__CICD_SCRIPT__</code></span>
  <span class="tag">临时页面</span>
</header>
<main>
  <section class="card">
    <div class="loghead">
      <strong>部署代码来源</strong>
      <button id="refresh-commits" style="margin-left:auto;padding:6px 12px;font-size:12px;">刷新</button>
    </div>
    <div id="commits">加载中…</div>
  </section>

  <section class="card">
    <div class="status">
      <span class="dot" id="dot"></span>
      <span class="label" id="state-label">正在读取状态…</span>
      <span class="muted" id="state-meta"></span>
    </div>
    <div class="row">
      <button id="deploy">Deploy latest code</button>
      <span class="muted">点击后在部署机（本机）执行 <code>__CICD_SCRIPT__</code>，输出实时显示在下方。</span>
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
  var wasRunning = false;

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
    if (wasRunning && !state.running && state.rc !== null) { loadCommits(); }
    wasRunning = state.running;
  }

  function renderCommits(data) {
    var html = "";
    [["substrate", "主仓库 substrate（deploy.sh / admin / nginx / 模板）"],
     ["workload", "工作负载 miaofang-agent（镜像代码）"]].forEach(function (pair) {
      var key = pair[0], title = pair[1];
      var r = data[key] || {};
      html += '<div class="repo">';
      html += '<div class="repo-head"><span class="repo-name">' + esc(title) + '</span>';
      if (r.branch) { html += ' <span class="muted">' + esc(r.dir) + ' · ' + esc(r.branch) + '</span>'; }
      html += '</div>';
      if (r.error) {
        html += '<div class="muted">' + esc(r.error) + '</div>';
      } else {
        if (r.behind !== null && r.behind > 0) {
          html += '<div class="behind">⚠ 远程有 ' + r.behind + ' 个提交未拉取（点 Deploy 前会先 git pull）'
                + (r.ahead ? '，本地领先 ' + r.ahead : '') + '</div>';
        } else if (r.behind === 0) {
          html += '<div class="uptodate">✔ 已与远程同步</div>';
        }
        if (r.commits && r.commits.length) {
          html += '<ul class="commit-list">';
          r.commits.forEach(function (c) {
            var head = c.refs && c.refs.indexOf("HEAD") >= 0;
            html += '<li class="' + (head ? 'head' : '') + '">'
                  + '<span class="ch">' + esc(c.short) + '</span> '
                  + '<span class="cs">' + esc(c.subject) + '</span> '
                  + '<span class="muted">' + esc(c.author) + ' · ' + esc(c.rel) + '</span>'
                  + '</li>';
          });
          html += '</ul>';
        } else if (r.commits) {
          html += '<div class="muted">无提交</div>';
        }
      }
      html += '</div>';
    });
    document.getElementById("commits").innerHTML = html;
  }

  function loadCommits() {
    fetch(BASE + "/commits", { cache: "no-store" })
      .then(function (r) { return r.json(); })
      .then(renderCommits)
      .catch(function () { document.getElementById("commits").textContent = "无法加载提交信息"; });
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

  loadCommits();
  poll();
})();
</script>
</body>
</html>
"""


def main(argv):
    # This file is the server process, not the CLI. People reasonably type
    # `./cicd_server.py stop` (note the underscore) and the argument used to be
    # ignored, so it tried to bind the port again and died with a raw
    # "Address already in use" traceback. Point them at the wrapper instead.
    if len(argv) > 1:
        sys.stderr.write(
            "cicd_server.py is the server process and takes no arguments.\n"
            "Use the wrapper instead:\n"
            "  ./cicd-server.sh start|stop|status|logs\n"
            "(note the dash: cicd-server.sh, not cicd_server.py)\n")
        return 2
    script_path = os.path.join(DEMO_DIR, os.path.basename(SCRIPT))
    if not os.path.exists(script_path):
        sys.stderr.write("deploy script %r not found (%s)\n" % (SCRIPT, DEMO_DIR))
        return 1
    try:
        srv = ThreadingHTTPServer(("127.0.0.1", PORT), Handler)
    except OSError as exc:
        if exc.errno in (errno.EADDRINUSE, errno.EACCES):
            sys.stderr.write(
                "cannot listen on 127.0.0.1:%d (%s).\n"
                "Another instance is probably already serving this page; check with:\n"
                "  ./cicd-server.sh status\n" % (PORT, exc.strerror))
            return 1
        raise
    srv.daemon_threads = True
    print("mfpi-cicd listening on http://127.0.0.1:%d/ (execute %s in %s)"
          % (PORT, SCRIPT, DEMO_DIR), flush=True)
    srv.serve_forever()
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
