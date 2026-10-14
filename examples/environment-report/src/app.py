"""Example application: an order API and a queue worker sharing one SQLite DB.

The processes report identity from their own execution path: the build is the
digest of the file this interpreter loaded, the DB and fixture come from the
same connection the API uses, and the worker stamps each order it processes
with its own instance and build.
"""
import hashlib
import http.server
import json
import os
import pathlib
import sqlite3
import sys
import time

LOADED = pathlib.Path(__file__).resolve()
BUILD = "sha256:" + hashlib.sha256(LOADED.read_bytes()).hexdigest()
DB = os.environ["APP_DB"]
QUEUE = os.environ["APP_QUEUE"]
INSTANCE = os.environ.get("PROC_COMPOSE_RUN_ID", "")


def order_total(qty, price):
    return qty * price


def connect():
    return sqlite3.connect(DB, timeout=5)


def identity():
    with connect() as c:
        rows = dict(c.execute("SELECT key, value FROM meta").fetchall())
    return {
        "instance": INSTANCE,
        "build": BUILD,
        "db": os.path.realpath(DB),
        "fixture_run": rows.get("fixture_run"),
        "fixture_version": rows.get("fixture_version"),
        "queue": os.path.realpath(QUEUE),
    }


class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def reply(self, code, body):
        data = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        if self.path == "/health":
            return self.reply(200, {"ok": True})
        if self.path == "/identity":
            return self.reply(200, identity())
        if self.path.startswith("/orders/"):
            with connect() as c:
                row = c.execute("SELECT status, total, worker FROM orders WHERE id=?", (int(self.path[8:]),)).fetchone()
            if row is None:
                return self.reply(404, {})
            return self.reply(200, {"status": row[0], "total": row[1], "worker": json.loads(row[2] or "null")})
        self.reply(404, {})

    def do_POST(self):
        if self.path != "/orders":
            return self.reply(404, {})
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        with connect() as c:
            if c.execute("SELECT 1 FROM products WHERE sku=?", (body["sku"],)).fetchone() is None:
                return self.reply(400, {"error": "unknown sku"})
            order = c.execute("INSERT INTO orders (sku, qty, status) VALUES (?, ?, 'pending')", (body["sku"], body["qty"])).lastrowid
        tmp = os.path.join(QUEUE, f".{order}.tmp")
        with open(tmp, "w") as f:
            f.write(str(order))
        os.rename(tmp, os.path.join(QUEUE, f"{order}.job"))
        self.reply(201, {"id": order})


def worker():
    stamp = json.dumps({"instance": INSTANCE, "build": BUILD})
    while True:
        for name in sorted(os.listdir(QUEUE)):
            if not name.endswith(".job"):
                continue
            path = os.path.join(QUEUE, name)
            with open(path) as f:
                order = int(f.read())
            with connect() as c:
                qty, price = c.execute("SELECT o.qty, p.price FROM orders o JOIN products p ON p.sku=o.sku WHERE o.id=?", (order,)).fetchone()
                c.execute("UPDATE orders SET status='done', total=?, worker=? WHERE id=?", (order_total(qty, price), stamp, order))
            os.remove(path)
        time.sleep(0.05)


if __name__ == "__main__":
    if sys.argv[1] == "api":
        http.server.ThreadingHTTPServer(("127.0.0.1", int(os.environ["PORT"])), Handler).serve_forever()
    worker()
