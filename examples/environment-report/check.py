"""Check: an order for 3 widgets is processed with a total of 750.

  check.py plain       the semantic assertion only
  check.py env-bound   validate the environment before and after the same
                       assertion, and write PROC_COMPOSE_ENVIRONMENT_REPORT

env-bound exits 0 when the order is right, 1 when it is wrong, and 3 when the
environment is wrong before the assertion, which then never runs, so it cannot
write to an environment it does not own. The report always covers every name
in REQUIRED: evidence that could not be read is reported as unobserved, never
left out. With EVIDENCE_DIR set, the expectation file is copied there.

Both modes honor the same pilot ordering (see pilot_sync.py):
PILOT_WAIT_BEFORE_CHECK before anything, then PILOT_SIGNAL_CHECKED and
PILOT_WAIT_AFTER_CHECK right after the assertion.
"""
import json
import os
import pathlib
import shutil
import sys
import time
import urllib.request

import pilot_sync

FIELDS = ("instance", "build", "db", "queue", "fixture_run", "fixture_version")
REQUIRED = (
    ["expected.run_id"]
    + [f"before.{f}" for f in FIELDS]
    + ["worker.instance", "worker.build"]
    + [f"after.{f}" for f in FIELDS]
)

mode = sys.argv[1]
url = os.environ.get("APP_URL") or os.environ["PROC_COMPOSE_URL_API_HTTP"]
run_id = os.environ["PROC_COMPOSE_RUN_ID"]
expected_path = pathlib.Path(os.environ["PROC_COMPOSE_SESSION_DIR"], "expected.json")
observations = []


def get(path):
    with urllib.request.urlopen(url + path, timeout=5) as r:
        return json.loads(r.read())


def observe(name, expected, observed):
    if observed is None:
        status = "unobserved"
    else:
        status = "matched" if observed == expected else "mismatched"
    observations.append({"name": name, "status": status, "expected": expected, "observed": observed})


def eligible():
    return all(o["status"] == "matched" for o in observations)


def finish(code, verdict):
    seen = {o["name"] for o in observations}
    for name in REQUIRED:
        if name not in seen:
            observations.append({"name": name, "status": "unobserved", "expected": None, "observed": None})
    report = os.environ.get("PROC_COMPOSE_ENVIRONMENT_REPORT")
    if report:
        tmp = report + ".tmp"
        pathlib.Path(tmp).write_text(json.dumps({"observations": observations}))
        os.rename(tmp, report)
    if os.environ.get("EVIDENCE_DIR") and expected_path.exists():
        shutil.copy(expected_path, pathlib.Path(os.environ["EVIDENCE_DIR"], "expected.json"))
    print(json.dumps({"verdict": verdict, "unmatched": [o["name"] for o in observations if o["status"] != "matched"]}))
    sys.exit(code)


def validate(stage, expected):
    try:
        ident = get("/identity")
    except Exception as exc:
        ident = {}
        print(f"{stage}: identity unavailable: {exc}", file=sys.stderr)
    observe(f"{stage}.instance", run_id, ident.get("instance"))
    for field in FIELDS[1:]:
        want = run_id if field == "fixture_run" else expected.get(field)
        observe(f"{stage}.{field}", want, ident.get(field))


pilot_sync.wait("PILOT_WAIT_BEFORE_CHECK")
expected = {}
if mode == "env-bound":
    try:
        expected = json.loads(expected_path.read_text())
    except (OSError, ValueError) as exc:
        print(f"expectation unavailable: {exc}", file=sys.stderr)
    observe("expected.run_id", run_id, expected.get("run_id"))
    validate("before", expected)
    if not eligible():
        finish(3, "environment_ineligible")

# The semantic assertion.
request = urllib.request.Request(url + "/orders", data=json.dumps({"sku": "widget", "qty": 3}).encode(), method="POST")
order = json.loads(urllib.request.urlopen(request, timeout=5).read())["id"]
deadline = time.time() + 10
while time.time() < deadline:
    state = get(f"/orders/{order}")
    if state["status"] == "done":
        break
    time.sleep(0.05)
else:
    state = {"status": "pending", "total": None, "worker": None}
passed = state["status"] == "done" and state["total"] == 750
pilot_sync.signal("PILOT_SIGNAL_CHECKED")
pilot_sync.wait("PILOT_WAIT_AFTER_CHECK")

if mode == "plain":
    print(json.dumps({"verdict": "passed" if passed else "failed", "order": state}))
    sys.exit(0 if passed else 1)

# This attempt's worker, on the expected build, must have processed the order.
worker = state.get("worker") or {}
observe("worker.instance", run_id, worker.get("instance"))
observe("worker.build", expected.get("build"), worker.get("build"))
validate("after", expected)
# The assertion ran, so its exit reports it; the report says whether to trust it.
finish(0 if passed else 1, ("passed" if passed else "failed") if eligible() else "environment_ineligible")
