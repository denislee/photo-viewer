#!/usr/bin/env python3
"""Self-contained tests for pv-face-detect's pipx venv selection (finding S-02).

These drive the *real* pv-face-detect.py end to end via subprocess against a
throwaway fake pipx tree, so they need neither face_recognition nor pytest
installed. Run directly:

    python3 scripts/test_pv_face_detect.py

or under pytest (the test_* functions are discovered automatically).

The trick: each fake venv's `bin/python` is a tiny shell script that

  * answers the selection probe -- invoked as `python -c "import face_recognition"`
    -- with exit 0 iff its venv name is listed in $FR_OK_VENVS, else exit 1; and
  * for any other argv (i.e. the actual re-exec `python <script> --check ...`)
    prints `REEXEC:<venv-name>` and exits 0.

So the marker on stdout tells us exactly which interpreter the script chose,
and the absence of a marker (with exit 2) proves it degraded to "unavailable"
rather than crashing or hanging.
"""
import os
import subprocess
import sys
import tempfile

SCRIPT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "pv-face-detect.py")

_FAKE_PY = """#!/bin/sh
# Fake venv python. Derive our venv name from .../venvs/<name>/bin/python.
name=$(basename "$(dirname "$(dirname "$0")")")
if [ "$1" = "-c" ]; then
    case "$2" in
        *"import face_recognition"*)
            case ":$FR_OK_VENVS:" in
                *":$name:"*) exit 0 ;;
                *) exit 1 ;;
            esac ;;
    esac
    exit 0
fi
# Re-exec path: the script is being run under this interpreter.
echo "REEXEC:$name"
exit 0
"""


def _make_tree(home, venvs):
    """Create fake pipx venvs (name -> bin/python) under a fake HOME."""
    base = os.path.join(home, ".local", "pipx", "venvs")
    for name in venvs:
        bindir = os.path.join(base, name, "bin")
        os.makedirs(bindir, exist_ok=True)
        py = os.path.join(bindir, "python")
        with open(py, "w") as fh:
            fh.write(_FAKE_PY)
        os.chmod(py, 0o755)


def _run(venvs, fr_ok):
    """Run the real script with a fake HOME/pipx tree. Returns (rc, stdout)."""
    with tempfile.TemporaryDirectory() as home:
        _make_tree(home, venvs)
        env = dict(os.environ)
        env["HOME"] = home
        env["FR_OK_VENVS"] = ":".join(fr_ok)
        env.pop("PV_FACE_REEXECED", None)
        # If face_recognition happens to be importable in the test runner's
        # own interpreter, the script returns before touching pipx at all and
        # these tests are moot -- skip loudly rather than pass vacuously.
        probe = subprocess.run(
            [sys.executable, "-c", "import face_recognition"],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        )
        if probe.returncode == 0:
            raise RuntimeError("face_recognition importable in test runner; cannot test re-exec")
        proc = subprocess.run(
            [sys.executable, SCRIPT, "--check"],
            env=env, capture_output=True, text=True, timeout=30,
        )
        return proc.returncode, proc.stdout


def _reexec_target(stdout):
    for line in stdout.splitlines():
        if line.startswith("REEXEC:"):
            return line[len("REEXEC:"):]
    return None


def test_prefers_named_face_recognition_venv():
    # face-recognition sorts AFTER ansible/black; the old code would have
    # execve'd ansible first. We must pick face-recognition regardless.
    rc, out = _run(["ansible", "black", "face-recognition", "zzz"],
                   fr_ok=["face-recognition"])
    assert _reexec_target(out) == "face-recognition", out


def test_underscore_named_venv_also_matches():
    rc, out = _run(["aaa", "face_recognition"], fr_ok=["face_recognition"])
    assert _reexec_target(out) == "face_recognition", out


def test_falls_back_to_probed_working_venv_when_no_named_one():
    # No venv is literally named face_recognition, but `myfaces` can import it.
    # The probe must find myfaces and skip ansible/black.
    rc, out = _run(["ansible", "black", "myfaces", "zzz"], fr_ok=["myfaces"])
    assert _reexec_target(out) == "myfaces", out


def test_named_but_broken_venv_is_skipped_for_a_working_one():
    # A face-recognition venv exists but its import fails (e.g. package removed
    # from the venv); another venv actually works. Probing must skip the broken
    # named one and land on the working fallback rather than stranding us.
    rc, out = _run(["face-recognition", "myfaces"], fr_ok=["myfaces"])
    assert _reexec_target(out) == "myfaces", out


def test_no_working_venv_degrades_to_unavailable_not_crash():
    # Nothing can import face_recognition. The script must NOT re-exec, and
    # must exit 2 (the "not importable" contract the Go Probe() relies on).
    rc, out = _run(["ansible", "black"], fr_ok=[])
    assert _reexec_target(out) is None, out
    assert rc == 2, (rc, out)


def test_no_pipx_dir_at_all_degrades_to_unavailable():
    rc, out = _run([], fr_ok=[])
    assert _reexec_target(out) is None, out
    assert rc == 2, (rc, out)


def _main():
    tests = [v for k, v in sorted(globals().items()) if k.startswith("test_")]
    failures = 0
    for t in tests:
        try:
            t()
            print("PASS", t.__name__)
        except Exception as e:  # noqa: BLE001
            failures += 1
            print("FAIL", t.__name__, "->", repr(e))
    print(f"\n{len(tests) - failures}/{len(tests)} passed")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(_main())
