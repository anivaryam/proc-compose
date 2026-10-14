"""Setup task and teardown for the example.

  prepare.py           build, refuse foreign state, reset and seed, and write
                       the expectation
  prepare.py release   remove this session's state (the teardown)

Both refuse to touch state outside the session directory. Pilot fault
switches: PILOT_SKIP_BUILD, PILOT_SKIP_SEED, PILOT_GUARD=off, PILOT_FAIL_SEED,
PILOT_KEEP_STATE, and PILOT_FAIL_TEARDOWN; PILOT_WAIT_BEFORE_SEED and
PILOT_SIGNAL_SEEDED order a reset against another session (see pilot_sync.py).
"""
import hashlib
import json
import os
import pathlib
import shutil
import sqlite3
import sys

import pilot_sync

here = pathlib.Path(__file__).resolve().parent
run_id = os.environ["PROC_COMPOSE_RUN_ID"]
session_dir = os.path.realpath(os.environ["PROC_COMPOSE_SESSION_DIR"])
db = os.path.realpath(os.environ["APP_DB"])
queue = os.path.realpath(os.environ["APP_QUEUE"])


def refuse_foreign_state(action):
    if os.environ.get("PILOT_GUARD") == "off":
        return
    for label, path in (("APP_DB", db), ("APP_QUEUE", queue)):
        if not path.startswith(session_dir + os.sep):
            sys.exit(f"{action}: refusing to touch {label}={path}: not inside session dir {session_dir}")


if sys.argv[1:] == ["release"]:
    if os.environ.get("PILOT_FAIL_TEARDOWN"):
        sys.exit("release: failed (injected)")
    if not os.environ.get("PILOT_KEEP_STATE"):
        refuse_foreign_state("release")
        shutil.rmtree(queue, ignore_errors=True)
        if os.path.exists(db):
            os.remove(db)
    sys.exit(0)

# 1. Build. Copying stands in for compiling; a skipped build leaves a stale one.
(here / "build").mkdir(exist_ok=True)
if not os.environ.get("PILOT_SKIP_BUILD"):
    shutil.copy(here / "src" / "app.py", here / "build" / "app.py")

# 2. Before anything destructive: state must live in this session.
refuse_foreign_state("prepare")
if os.environ.get("PILOT_FAIL_SEED"):
    sys.exit("prepare: seed failed (injected)")
pilot_sync.wait("PILOT_WAIT_BEFORE_SEED")

# 3. Reset and seed, stamping the fixture with this attempt's run ID.
if not (os.environ.get("PILOT_SKIP_SEED") and os.path.exists(db)):
    if os.path.exists(db):
        os.remove(db)
    with sqlite3.connect(db) as c:
        c.executescript(
            "CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT);"
            "CREATE TABLE products (sku TEXT PRIMARY KEY, price INTEGER);"
            "CREATE TABLE orders (id INTEGER PRIMARY KEY, sku TEXT, qty INTEGER, status TEXT, total INTEGER, worker TEXT);"
            "INSERT INTO products VALUES ('widget', 250), ('gadget', 1000);"
        )
        c.executemany("INSERT INTO meta VALUES (?, ?)", [("fixture_run", run_id), ("fixture_version", "1")])
shutil.rmtree(queue, ignore_errors=True)
os.makedirs(queue)
pilot_sync.signal("PILOT_SIGNAL_SEEDED")

# 4. What this attempt expects the services and the check to observe.
expected = {
    "run_id": run_id,
    "build": "sha256:" + hashlib.sha256((here / "src" / "app.py").read_bytes()).hexdigest(),
    "db": db,
    "queue": queue,
    "fixture_version": "1",
}
pathlib.Path(session_dir, "expected.json").write_text(json.dumps(expected))
print("prepare: ok", json.dumps(expected))
