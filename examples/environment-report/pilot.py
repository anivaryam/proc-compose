"""Pilot for environment reports: seeded faults, acceptance gates, and an
evidence handoff that is read after the runtime directory is gone.

  python3 pilot.py [--bin BIN] [--baseline-bin BIN] [--work DIR]

Every scenario runs in two modes. "baseline" runs `check.py plain` with
--baseline-bin (today's workflow). "env-bound" runs `check.py env-bound` with
--require-environment. All sessions use a runtime directory the pilot creates
(XDG_RUNTIME_DIR) and removes at the end. Results, workspaces, and the evidence
bundle stay in --work. Exits 1 if any gate fails.
"""
import argparse
import json
import os
import pathlib
import shutil
import signal
import subprocess
import sys
import tempfile
import threading

HERE = pathlib.Path(__file__).resolve().parent
MODES = ("baseline", "env-bound")


class Pilot:
    def __init__(self, args):
        self.bin, self.baseline_bin = args.bin, args.baseline_bin or args.bin
        self.work = pathlib.Path(args.work or tempfile.mkdtemp(prefix="pc-pilot-work.")).resolve()
        self.work.mkdir(parents=True, exist_ok=True)
        # Unix socket paths are short, so the runtime directory cannot live in a deep work dir.
        self.runtime = pathlib.Path(tempfile.mkdtemp(prefix="pc-pilot.", dir=os.environ.get("XDG_RUNTIME_DIR") or None))
        self.env = dict(os.environ, XDG_RUNTIME_DIR=str(self.runtime))
        self.ws, self.out, self.sync = self.work / "ws", self.work / "results", self.work / "sync"
        for d in (self.ws, self.out, self.sync, self.work / "shared"):
            shutil.rmtree(d, ignore_errors=True)
            d.mkdir(parents=True)
        self.runs = {}
        self.order = {}
        self.gates = []
        self.holders = {}

    # Workspaces: one worktree per fault, copied from this directory.
    def workspace(self, name, src=None, build=None, edit_config=None):
        dst = self.ws / name
        shutil.copytree(HERE, dst, ignore=shutil.ignore_patterns("build", "__pycache__", "pilot.py", "read_evidence.py", "*.md", ".gitignore"))
        if src is not None:
            (dst / "src" / "app.py").write_text(src)
        if build is not None:
            (dst / "build").mkdir()
            (dst / "build" / "app.py").write_text(build)
        if edit_config:
            config = dst / "proc-compose.yml"
            config.write_text(edit_config(config.read_text()))

    def setup(self):
        good = (HERE / "src" / "app.py").read_text()
        regress = good.replace("return qty * price", "return qty + price")
        noident = good.replace('        if self.path == "/identity":\n            return self.reply(200, identity())\n', "")
        assert regress != good and noident != good
        self.regress = regress
        shared = self.work / "shared"
        outside = lambda text: text.replace("${session.dir}/app.db", f"{shared}/app.db").replace("${session.dir}/queue", f"{shared}/queue")
        self.workspace("good")
        self.workspace("regress", src=regress)
        self.workspace("noident", src=noident)
        self.workspace("stale", src=regress, build=good)  # source has the fix-to-be, build is old
        self.workspace("sharedA", edit_config=outside)
        self.workspace("sharedB", edit_config=outside)

    def cli(self, ws, session, *args, env=None, binary=None):
        argv = [binary or self.bin, "-f", str(self.ws / ws / "proc-compose.yml"), "--session", session, *args]
        proc = subprocess.Popen(argv, env=dict(self.env, **(env or {})), stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            stdout, stderr = proc.communicate(timeout=300)
        except BaseException:
            # Interrupt rather than kill: proc-compose then stops what it started.
            proc.send_signal(signal.SIGINT)
            proc.communicate(timeout=120)
            raise
        return subprocess.CompletedProcess(argv, proc.returncode, stdout, stderr)

    def live_sessions(self):
        if not self.runtime.exists():
            return []
        listing = self.cli("good", "pilot", "sessions", "--json")
        return [s["session"] for s in json.loads(listing.stdout)["sessions"] if s["live"]]

    def verify(self, mode, scenario, ws, session, env=None, check=None, required=None):
        required = mode == "env-bound" if required is None else required
        flags = ["--require-environment"] if required else []
        check = check or ("env-bound" if mode == "env-bound" else "plain")
        binary = self.baseline_bin if mode == "baseline" else self.bin
        proc = self.cli(ws, f"{mode}-{session}", "verify", "--json", "--cleanup-timeout", "30s", *flags,
                        "--", "python3", "check.py", check, env=env, binary=binary)
        return self.record(mode, scenario, proc)

    def record(self, mode, scenario, proc):
        (self.out / mode).mkdir(exist_ok=True)
        (self.out / mode / f"{scenario}.json").write_text(proc.stdout)
        (self.out / mode / f"{scenario}.stderr").write_text(proc.stderr)
        try:
            result = json.loads(proc.stdout)
        except ValueError:
            result = {"status": "unreadable", "stderr": proc.stderr[-500:]}
        result["_exit"] = proc.returncode
        self.runs[(mode, scenario)] = result
        return result

    def interleave(self, mode, tag, first, second, env):
        """Run two sessions at once in a fixed order, the same in both modes.

        second's setup resets its state only after first's check has made its
        assertion, first's check validates again only after that reset, and
        second's check starts only after first's attempt has ended. The marker
        files' times record the order that actually happened.
        """
        marks = {name: self.sync / f"{mode}-{tag}-{name}" for name in ("checked", "seeded", "done")}
        for mark in marks.values():
            mark.unlink(missing_ok=True)

        def run_first():
            try:
                self.verify(mode, f"{tag}-first", first, f"{tag}-1", env=dict(
                    env, PILOT_SIGNAL_CHECKED=str(marks["checked"]), PILOT_WAIT_AFTER_CHECK=str(marks["seeded"]), PILOT_KEEP_STATE="1"))
            finally:
                marks["done"].touch()

        thread = threading.Thread(target=run_first)
        thread.start()
        try:
            self.verify(mode, f"{tag}-second", second, f"{tag}-2", env=dict(
                env, PILOT_WAIT_BEFORE_SEED=str(marks["checked"]), PILOT_SIGNAL_SEEDED=str(marks["seeded"]), PILOT_WAIT_BEFORE_CHECK=str(marks["done"])))
        finally:
            thread.join()
        self.order[(mode, tag)] = [mark.stat().st_mtime_ns if mark.exists() else None for mark in marks.values()]

    def scenarios(self, mode):
        v = lambda *a, **k: self.verify(mode, *a, **k)
        v("S1-correct", "good", "s1")
        v("S2-known-regression", "regress", "s2")

        # S3a: the check reaches a dev server another worktree left running.
        dev = f"{mode}-dev"
        self.cli("good", dev, "up", "--silent")
        try:
            sessions = json.loads(self.cli("good", dev, "sessions", "--json").stdout)["sessions"]
            url = next(s["urls"]["api"]["http"] for s in sessions if s["session"] == dev)
            v("S3a-wrong-endpoint", "regress", "s3a", env={"APP_URL": url})
            # S3c: a worktree whose config hardcodes the port the dev server
            # already holds, as a README-style config does. Its readiness
            # probe is answered by the dev server before its own api binds.
            port = url.rsplit(":", 1)[1]
            hardcode = lambda text: (text.replace("    cmd: python3 build/app.py api\n", f"    cmd: PORT={port} python3 build/app.py api\n")
                                     .replace("    ports:\n      http: 18100\n", "")
                                     .replace("${urls.api.http}/health", f"http://127.0.0.1:{port}/health"))
            self.workspace(f"hardcoded-{mode}", src=self.regress, edit_config=hardcode)
            status = json.loads(self.cli("good", dev, "status", "--json").stdout)
            leader = next((p.get("pid") for p in status["processes"] if p["name"] == "api"), None)
            res = v("S3c-hardcoded-port-held", f"hardcoded-{mode}", "s3c", env={"APP_URL": url})
            # The holder is the dev api's server process, which may sit below
            # its shell: it belongs when it is in the dev api's process group,
            # read while the dev session still runs.
            owner = next((o.get("owner") for o in res.get("startup", {}).get("readiness_observations", []) if o["process"] == "api"), None)
            try:
                group = os.getpgid(owner["pid"]) if owner and owner.get("pid") else None
            except OSError:
                group = None
            self.holders[mode] = (leader, group)
            if mode == "env-bound":
                # S3d: the same worktree with foreign_listener: reject. The dev
                # server's answer is refused, so the check never runs against it.
                reject = lambda text: hardcode(text).replace("    depends_on: [prepare]\n    env: *state\n    ready_when:",
                                                             "    depends_on: [prepare]\n    env: *state\n    foreign_listener: reject\n    ready_timeout: 5\n    ready_when:")
                self.workspace("hardcoded-reject", src=self.regress, edit_config=reject)
                res = v("S3d-hardcoded-port-rejected", "hardcoded-reject", "s3d", env={"APP_URL": url})
                owner = next((o.get("owner") for o in res.get("startup", {}).get("readiness_observations", []) if o["process"] == "api"), None)
                try:
                    res["_holder_group"] = os.getpgid(owner["pid"]) if owner and owner.get("pid") else None
                except OSError:
                    res["_holder_group"] = None
        finally:
            self.cli("good", dev, "stop")
        v("S3b-stale-build", "stale", "s3b", env={"PILOT_SKIP_BUILD": "1"})

        # S4a: state configured outside the session; prepare must refuse before resetting it.
        sentinel = self.work / "shared" / "app.db"
        sentinel.write_text("not ours")
        v("S4a-foreign-state-guarded", "sharedA", "s4a")
        self.runs[(mode, "S4a-foreign-state-guarded")]["_sentinel_kept"] = sentinel.exists() and sentinel.read_text() == "not ours"

        # S4b: two sessions share state (guard off); the second resets it during the first's check.
        self.interleave(mode, "S4b-shared", "sharedA", "sharedB", {"PILOT_GUARD": "off"})
        self.interleave(mode, "S4b-shared-reversed", "sharedB", "sharedA", {"PILOT_GUARD": "off"})
        # S4c: the same order, but each session has its own state: no interference.
        self.interleave(mode, "S4c-isolated", "good", "good", {})

        # S5: one session name reused; the second attempt skips the reset.
        v("S5-reuse-first", "good", "s5", env={"PILOT_KEEP_STATE": "1"})
        v("S5-reuse-stale-fixture", "good", "s5", env={"PILOT_SKIP_SEED": "1"})
        v("S5-reuse-reset", "good", "s5")

        v("S6a-prepare-failed", "good", "s6a", env={"PILOT_FAIL_SEED": "1"})
        v("S6b-no-identity-endpoint", "noident", "s6b")
        v("S7-teardown-failed", "good", "s7", env={"PILOT_FAIL_TEARDOWN": "1"})
        if mode == "env-bound":
            v("S9-plain-check-required", "good", "s9", check="plain")

        # S8: compare a correct worktree with the stale-build worktree.
        flags = ["--require-environment"] if mode == "env-bound" else []
        proc = self.cli("good", f"{mode}-s8", "compare", "--json", *flags,
                        "--baseline", str(self.ws / "good"), "--candidate", str(self.ws / "stale"),
                        "--", "python3", "check.py", "env-bound" if mode == "env-bound" else "plain",
                        env={"PILOT_SKIP_BUILD": "1"}, binary=self.baseline_bin if mode == "baseline" else self.bin)
        self.record(mode, "S8-compare-good-vs-stale", proc)

    def export_evidence(self):
        """Export two attempts with existing commands, remove the runtime, then read them."""
        bundle = self.work / "evidence"
        shutil.rmtree(bundle, ignore_errors=True)
        ids = {}
        for name, ws, env in (("eligible", "good", {}), ("stale-build", "stale", {"PILOT_SKIP_BUILD": "1"})):
            attempt = bundle / name
            attempt.mkdir(parents=True)
            session = f"evidence-{name}"
            proc = self.cli(ws, session, "verify", "--json", "--require-environment", "--",
                            "python3", "check.py", "env-bound", env=dict(env, EVIDENCE_DIR=str(attempt)))
            (attempt / "verify.json").write_text(proc.stdout)
            ids[name] = (ws, session, json.loads(proc.stdout)["attempt_id"])
            why = self.cli(ws, session, "why", "--attempt", ids[name][2], "--json")
            (attempt / "why.json").write_text(why.stdout)

        live = self.live_sessions()
        self.gate("no live session before the runtime directory is removed", not live, live)
        if live:
            return
        shutil.rmtree(self.runtime)
        ws, session, attempt_id = ids["eligible"]
        gone = self.cli(ws, session, "why", "--attempt", attempt_id, "--json")
        self.gate("runtime records are gone: `why --attempt` no longer finds the attempt", gone.returncode != 0, gone.stderr.strip()[-200:])

        reader = subprocess.run([sys.executable, "-I", str(HERE / "read_evidence.py"), str(bundle), "--json"],
                                capture_output=True, text=True, env={"PATH": os.environ.get("PATH", "")})
        self.gate("evidence bundle readable without proc-compose or its runtime", reader.returncode == 0, reader.stderr[-200:])
        if reader.returncode != 0:
            return
        read = {a["attempt"]: a for a in json.loads(reader.stdout)}
        (self.work / "evidence-read.json").write_text(reader.stdout)
        eligible, stale = read["eligible"], read["stale-build"]
        self.gate("bundle: eligible attempt shows expectation, matched observations, passed check, verified cleanup",
                  eligible["expected"] is not None and eligible["environment"] == "eligible" and eligible["check"] == "passed"
                  and eligible["cleanup"] == "passed" and eligible["termination_verified"] is True and eligible["processes"],
                  eligible)
        self.gate("bundle: stale attempt names the build mismatch against the retained expectation",
                  stale["environment"] == "ineligible" and stale["expected"] is not None
                  and any(o["name"] == "before.build" and o["expected"] == stale["expected"]["build"] for o in stale["unmatched"]),
                  [o["name"] for o in stale["unmatched"]])
        self.gate("bundle: gaps are stated (check-reported observations, unproven loaded code)",
                  all(any("reported by the check" in g for g in a["gaps"]) and any("loaded code" in g for g in a["gaps"]) for a in read.values()),
                  [a["gaps"] for a in read.values()])

        # The reader must not hide incomplete history: set each diagnosis flag on its own copy.
        flagged = self.work / "evidence-flagged"
        shutil.rmtree(flagged, ignore_errors=True)
        for flag in ("gap", "truncated"):
            shutil.copytree(bundle / "eligible", flagged / flag)
            why = json.loads((flagged / flag / "why.json").read_text())
            why[flag] = True
            (flagged / flag / "why.json").write_text(json.dumps(why))
        reader = subprocess.run([sys.executable, "-I", str(HERE / "read_evidence.py"), str(flagged), "--json"],
                                capture_output=True, text=True, env={"PATH": os.environ.get("PATH", "")})
        gaps = {a["attempt"]: " | ".join(a["gaps"]) for a in json.loads(reader.stdout or "[]")}
        self.gate("bundle: the reader reports a journal gap and truncated history, each only when flagged",
                  "journal has a gap" in gaps.get("gap", "") and "journal has a gap" not in gaps.get("truncated", "")
                  and "history truncated" in gaps.get("truncated", "") and "history truncated" not in gaps.get("gap", ""), gaps)

    def gate(self, description, ok, detail=None):
        self.gates.append({"gate": description, "passed": bool(ok), "detail": None if ok else detail})

    def check_gates(self):
        r = lambda scenario, mode="env-bound": self.runs[(mode, scenario)]
        env = lambda res: (res.get("environment") or {}).get("status")
        unmatched = lambda res: {o["name"] for o in (res.get("environment") or {}).get("observations", []) if o["status"] != "matched"}
        codes = lambda res: {e["code"] for e in res.get("errors", [])}
        clean = lambda res: res.get("cleanup", {}).get("termination_verified") is True

        # The baseline must show the problem, or the pilot proves nothing.
        for scenario in ("S3a-wrong-endpoint", "S3b-stale-build", "S5-reuse-stale-fixture", "S6b-no-identity-endpoint"):
            self.gate(f"baseline: {scenario} passes silently", r(scenario, "baseline")["status"] == "passed", r(scenario, "baseline")["status"])
        self.gate("baseline: S8 compares the stale build as comparable", r("S8-compare-good-vs-stale", "baseline")["status"] == "comparable")

        self.gate("S1: correct environment passes as eligible", r("S1-correct")["status"] == "passed" and env(r("S1-correct")) == "eligible")
        s2 = r("S2-known-regression")
        self.gate("S2: known regression fails the check in an eligible environment",
                  s2["check"]["status"] == "failed" and env(s2) == "eligible" and not codes(s2) & {"environment_ineligible", "environment_unverified"}, codes(s2))
        for scenario, names in (("S3a-wrong-endpoint", {"before.instance", "before.db"}),
                                ("S3b-stale-build", {"before.build"}),
                                ("S5-reuse-stale-fixture", {"before.fixture_run"}),
                                ("S6b-no-identity-endpoint", {"before.instance"})):
            res = r(scenario)
            self.gate(f"{scenario}: environment_ineligible naming {sorted(names)}",
                      env(res) == "ineligible" and "environment_ineligible" in codes(res) and names <= unmatched(res), sorted(unmatched(res)))
        s4a = r("S4a-foreign-state-guarded")
        self.gate("S4a: setup and teardown refuse foreign state; it is untouched", "startup_failed" in codes(s4a) and env(s4a) == "not_run" and s4a["_sentinel_kept"])
        for (mode, tag), stamps in sorted(self.order.items()):
            self.gate(f"{mode} {tag}: first's assertion, then second's reset, then first's end",
                      None not in stamps and stamps == sorted(stamps), stamps)
        for tag in ("S4b-shared", "S4b-shared-reversed"):
            first, second = r(f"{tag}-first"), r(f"{tag}-second")
            self.gate(f"baseline {tag}: both sessions pass, although the first's state was reset under it",
                      r(f"{tag}-first", "baseline")["status"] == r(f"{tag}-second", "baseline")["status"] == "passed")
            self.gate(f"{tag}: the first session, whose state was reset under it, is ineligible on after.fixture_run only",
                      env(first) == "ineligible" and first["check"]["status"] == "passed" and unmatched(first) == {"after.fixture_run"}, sorted(unmatched(first)))
            self.gate(f"{tag}: the second session, which ran alone on the shared state, passes as eligible",
                      second["status"] == "passed" and env(second) == "eligible", (second["status"], sorted(unmatched(second))))
        a, b = r("S4c-isolated-first"), r("S4c-isolated-second")
        self.gate("baseline S4c: both isolated sessions pass", r("S4c-isolated-first", "baseline")["status"] == r("S4c-isolated-second", "baseline")["status"] == "passed")
        dbs = {o["observed"] for res in (a, b) for o in res["environment"]["observations"] if o["name"] == "after.db"}
        self.gate("S4c: concurrent isolated sessions are both eligible on distinct databases",
                  env(a) == env(b) == "eligible" and a["status"] == b["status"] == "passed" and len(dbs) == 2, dbs)
        self.gate("S5: the first and the reset reuse of a session name are eligible",
                  env(r("S5-reuse-first")) == env(r("S5-reuse-reset")) == "eligible" and r("S5-reuse-reset")["status"] == "passed")
        s6a = r("S6a-prepare-failed")
        self.gate("S6a: failed preparation is startup_failed and the check never ran", "startup_failed" in codes(s6a) and env(s6a) == "not_run")
        s7 = r("S7-teardown-failed")
        self.gate("S7: teardown failure is separate from verified termination and an eligible environment",
                  "teardown_failed" in codes(s7) and s7["cleanup"]["status"] == "passed" and env(s7) == "eligible", codes(s7))
        s9 = r("S9-plain-check-required")
        self.gate("S9: a check that writes no report cannot pass --require-environment", "environment_unverified" in codes(s9))
        s8 = r("S8-compare-good-vs-stale")
        self.gate("S8: compare refuses the stale-build side", s8["status"] == "not_comparable" and "environment_ineligible" in codes(s8), s8.get("errors"))
        for (mode, scenario), res in sorted(self.runs.items()):
            if "cleanup" in res:
                self.gate(f"{mode} {scenario}: termination verified", clean(res))
        self.check_owner_gates(r)

    def check_owner_gates(self, r):
        """Readiness attribution (Linux): who answered each passing http probe."""
        if sys.platform not in ("linux", "win32"):
            return
        owners = {}
        for (mode, scenario), res in sorted(self.runs.items()):
            if mode != "env-bound":
                continue
            for obs in (res.get("startup") or {}).get("readiness_observations", []):
                if obs["status"] == "passed" and obs.get("probe") in ("http", "tcp"):
                    owners[(scenario, obs["process"])] = obs.get("owner")
        s3c = owners.pop(("S3c-hardcoded-port-held", "api"), None)
        self.gate("baseline: S3c's readiness passes against the dev server",
                  any(o["process"] == "api" and o["status"] == "passed" for o in r("S3c-hardcoded-port-held", "baseline")["startup"]["readiness_observations"]))
        leader, group = self.holders.get("env-bound", (None, None))
        self.gate("S3c: readiness answered by the dev server is foreign, naming a process of the dev api",
                  s3c is not None and s3c["result"] == "foreign" and leader is not None and group == leader, (s3c, leader, group))
        s3d = r("S3d-hardcoded-port-rejected")
        refused = next((o.get("owner") for o in s3d["startup"]["readiness_observations"] if o["process"] == "api"), None)
        self.gate("S3d: foreign_listener: reject refuses the dev server; startup fails, the check never runs, and the receipt names a process of the dev api",
                  "startup_failed" in {e["code"] for e in s3d.get("errors", [])} and s3d["check"]["status"] == "not_run"
                  and refused is not None and refused["result"] == "foreign" and s3d.get("_holder_group") == leader,
                  (s3d.get("errors"), s3d["check"]["status"], refused, s3d.get("_holder_group"), leader))
        wrong = {f"{scenario}/{process}": o for (scenario, process), o in owners.items() if not o or o["result"] != "owned"}
        self.gate(f"every other passing readiness probe ({len(owners)}) is owned: no false foreign", owners and not wrong, wrong)

    def table(self):
        for (mode, scenario), res in sorted(self.runs.items()):
            if res.get("kind") == "verify.comparison":
                sides = [f"{side}:{x['check_status']}/{x.get('environment_status')}" for side, s in res["sides"].items() for x in s["attempts"]]
                print(f"{mode:9} {scenario:30} compare={res['status']} equal={res['outcomes_equal']} {sides} errors={[e['code'] for e in res['errors']]}")
                continue
            e = res.get("environment") or {}
            bad = [o["name"] for o in e.get("observations", []) if o["status"] != "matched"]
            print(f"{mode:9} {scenario:30} {res['status']:7} startup={res.get('startup', {}).get('status')} "
                  f"check={res.get('check', {}).get('status')}({res.get('check', {}).get('exit_code')}) env={e.get('status', '-')} "
                  f"errors={[x['code'] for x in res.get('errors', [])]}" + (f" unmatched={bad[:4]}{'...' if len(bad) > 4 else ''}" if bad else ""))


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--bin", default="proc-compose", help="proc-compose with environment reports")
    parser.add_argument("--baseline-bin", help="proc-compose for baseline mode (default: --bin)")
    parser.add_argument("--work", help="directory for workspaces, results, and evidence (default: a new temporary directory)")
    pilot = Pilot(parser.parse_args())
    try:
        pilot.setup()
        for mode in MODES:
            pilot.scenarios(mode)
        pilot.check_gates()
        pilot.export_evidence()
    finally:
        # Removing the runtime directory of a live session would orphan it.
        try:
            live = pilot.live_sessions()
        except Exception as exc:
            live = [f"unknown ({exc})"]
        if live:
            print(f"keeping {pilot.runtime}: live sessions {live}; stop them with XDG_RUNTIME_DIR={pilot.runtime} proc-compose ... stop", file=sys.stderr)
        else:
            shutil.rmtree(pilot.runtime, ignore_errors=True)
    pilot.table()
    (pilot.work / "gates.json").write_text(json.dumps(pilot.gates, indent=2))
    failed = [g for g in pilot.gates if not g["passed"]]
    for g in pilot.gates:
        print(("PASS " if g["passed"] else "FAIL ") + g["gate"] + ("" if g["passed"] else f"  {g['detail']}"))
    print(f"{len(pilot.gates) - len(failed)}/{len(pilot.gates)} gates passed; results in {pilot.work}")
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()
