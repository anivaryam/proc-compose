"""Read an exported evidence bundle without proc-compose or its runtime directory.

  read_evidence.py BUNDLE [--json]

BUNDLE holds one directory per attempt with verify.json (the `verify --json`
response), why.json (`why --attempt <id> --json`), and expected.json (the
attempt's expectation, copied by the check). Exits 1 if an attempt is missing
verify.json; any other missing piece is reported as a gap.
"""
import json
import pathlib
import sys


def load(path):
    try:
        return json.loads(path.read_text())
    except (OSError, ValueError):
        return None


def read(attempt_dir):
    result = load(attempt_dir / "verify.json")
    if result is None:
        return None
    why = load(attempt_dir / "why.json")
    expected = load(attempt_dir / "expected.json")
    env, cleanup, source = result["environment"], result["cleanup"], result["source"]
    gaps = ["observations are reported by the check, not by proc-compose"]
    if not source["loaded_code_proven"]:
        gaps.append("loaded code not proven by proc-compose (source revision is provenance only)")
    if source["revision"] is None:
        gaps.append("no source revision recorded (not a git worktree, or git unavailable)")
    if expected is None:
        gaps.append("expected.json missing: expectations only as recorded in observations")
    if why is None:
        gaps.append("why.json missing: no process history")
    else:
        gaps += [f"history: {limit}" for limit in why["limitations"]]
        if why["history_missing"]:
            gaps.append("history: missing or rotated")
        if why["gap"]:
            gaps.append("history: the journal has a gap in this attempt's events")
        if why["truncated"]:
            gaps.append("history truncated at the diagnosis limits")
    if result["evidence"]["gap"] or result["evidence"]["truncated"]:
        gaps.append("journal excerpt has a gap or was truncated")
    return {
        "attempt": attempt_dir.name,
        "attempt_id": result["attempt_id"],
        "session": result["session"],
        "status": result["status"],
        "errors": [e["code"] for e in result["errors"]],
        "revision": source["revision"],
        "dirty": source["dirty"],
        "expected": expected,
        "environment": env["status"],
        "environment_reason": env["reason"],
        "environment_required": env["required"],
        "unmatched": [o for o in env["observations"] if o["status"] != "matched"],
        "observations": len(env["observations"]),
        "startup": result["startup"]["status"],
        "check": result["check"]["status"],
        "check_exit": result["check"]["exit_code"],
        "cleanup": cleanup["status"],
        "termination_verified": cleanup["termination_verified"],
        "teardown": {t["name"]: t["status"] for t in cleanup["teardown"]},
        "processes": [p["name"] for p in (why or {}).get("processes", [])],
        "gaps": gaps,
    }


def main():
    bundle = pathlib.Path(sys.argv[1])
    attempts = []
    for attempt_dir in sorted(p for p in bundle.iterdir() if p.is_dir()):
        summary = read(attempt_dir)
        if summary is None:
            print(f"{attempt_dir.name}: verify.json missing or unreadable", file=sys.stderr)
            sys.exit(1)
        attempts.append(summary)
    if "--json" in sys.argv:
        print(json.dumps(attempts, indent=2))
        return
    for a in attempts:
        print(f"{a['attempt']} ({a['attempt_id']}, session {a['session']}): {a['status']} {a['errors']}")
        print(f"  source: {a['revision']} dirty={a['dirty']}")
        print(f"  expected: {json.dumps(a['expected'])}")
        print(f"  environment: {a['environment']} reason={a['environment_reason']} required={a['environment_required']} "
              f"({a['observations']} observations, {len(a['unmatched'])} not matched)")
        for o in a["unmatched"]:
            print(f"    {o['name']} {o['status']}: expected {o['expected']!r}, observed {o['observed']!r}")
        print(f"  semantic: startup={a['startup']} check={a['check']} exit={a['check_exit']}")
        print(f"  cleanup: {a['cleanup']} termination_verified={a['termination_verified']} teardown={a['teardown']}")
        print(f"  history: processes {a['processes']}")
        for gap in a["gaps"]:
            print(f"  gap: {gap}")


if __name__ == "__main__":
    main()
