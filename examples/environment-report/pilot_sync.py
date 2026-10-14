"""Pilot ordering between two sessions: one waits for a file the other creates.

Each function takes the name of an environment variable holding the file path
and does nothing when the variable is unset, so ordinary runs are unaffected.
"""
import os
import pathlib
import sys
import time

TIMEOUT = 60


def signal(var):
    if os.environ.get(var):
        pathlib.Path(os.environ[var]).touch()


def wait(var):
    path = os.environ.get(var)
    if not path:
        return
    deadline = time.time() + TIMEOUT
    while not os.path.exists(path):
        if time.time() > deadline:
            sys.exit(f"{var}: {path} did not appear within {TIMEOUT}s")
        time.sleep(0.05)
