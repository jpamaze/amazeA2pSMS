#!/usr/bin/env python3
"""Development stand-in for the ClickHouse HTTP interface, backed by chdb
(the embedded ClickHouse engine). For local runs without Docker only.

    pip install chdb
    python3 scripts/ch-dev-server.py --port 8123 --path .dev/chdb

Supports what ch-writer and ad-hoc queries need: ?query=... with an optional
request body (data for INSERT ... FORMAT), ?database=..., and settings passed
as query parameters. Results come back as TabSeparated.
"""
import argparse
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

import chdb.session as chs

RESERVED = {"query", "database", "user", "password", "default_format"}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, default=8123)
    ap.add_argument("--path", default=".dev/chdb")
    ap.add_argument("--database", default="sms")
    args = ap.parse_args()

    sess = chs.Session(args.path)
    sess.query(f"CREATE DATABASE IF NOT EXISTS {args.database}")
    lock = threading.Lock()  # chdb sessions are not thread-safe

    class Handler(BaseHTTPRequestHandler):
        def _run(self):
            u = urlparse(self.path)
            params = {k: v[-1] for k, v in parse_qs(u.query).items()}
            n = int(self.headers.get("Content-Length") or 0)
            body = self.rfile.read(n).decode() if n else ""
            query = params.get("query", "")
            if not query:  # ClickHouse also accepts the query in the body
                query, body = body, ""
            if u.path == "/ping":
                return self._send(200, "Ok.\n")
            settings = [f"{k} = '{v}'" for k, v in params.items() if k not in RESERVED]
            sql = query
            if settings and query.strip().upper().startswith(("SELECT", "INSERT")):
                if "FORMAT" in query.upper() and query.strip().upper().startswith("INSERT"):
                    head, _, fmt = query.rpartition("FORMAT")
                    sql = f"{head} SETTINGS {', '.join(settings)} FORMAT{fmt}"
            if body:
                sql = sql + "\n" + body
            db = params.get("database", args.database)
            try:
                with lock:
                    sess.query(f"USE {db}")
                    res = sess.query(sql, "TabSeparated")
                out = res.bytes().decode() if hasattr(res, "bytes") else str(res)
                self._send(200, out)
            except Exception as e:  # noqa: BLE001 - report every engine error to the client
                self._send(500, f"Code: 0. DB::Exception: {e}\n")

        def _send(self, code, text):
            data = text.encode()
            self.send_response(code)
            self.send_header("Content-Type", "text/tab-separated-values; charset=UTF-8")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        do_GET = _run
        do_POST = _run

        def log_message(self, *a):
            pass

    print(f"ch-dev-server on :{args.port} (chdb, path {args.path})", flush=True)
    ThreadingHTTPServer(("", args.port), Handler).serve_forever()


if __name__ == "__main__":
    main()
