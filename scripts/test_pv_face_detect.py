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
import time

SCRIPT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "pv-face-detect.py")

_FAKE_PY = """#!/bin/sh
# Fake venv python. Derive our venv name from .../venvs/<name>/bin/python.
name=$(basename "$(dirname "$(dirname "$0")")")
if [ "$1" = "-c" ]; then
    case "$2" in
        *"import face_recognition"*)
            case ":$FR_HANG_VENVS:" in
                *":$name:"*) exec sleep 30 ;;
            esac
            case ":$FR_OK_VENVS:" in
                *":$name:"*) exit 0 ;;
                *) echo "ImportError: fake $name" >&2; exit 1 ;;
            esac ;;
    esac
    exit 0
fi
# Re-exec path: the script is being run under this interpreter.
echo "REEXEC:$name"
exit 0
"""


# A venv name that _make_tree places where `make face-deps` builds its venv
# ($HOME/.local/share/photo-viewer/face-venv) instead of under pipx.
OWN_VENV = "face-venv"


def _make_tree(home, venvs):
    """Create fake pipx venvs (name -> bin/python) under a fake HOME;
    OWN_VENV goes to the face-deps location instead."""
    base = os.path.join(home, ".local", "pipx", "venvs")
    for name in venvs:
        root = os.path.join(home, ".local", "share", "photo-viewer") if name == OWN_VENV else base
        bindir = os.path.join(root, name, "bin")
        os.makedirs(bindir, exist_ok=True)
        py = os.path.join(bindir, "python")
        with open(py, "w") as fh:
            fh.write(_FAKE_PY)
        os.chmod(py, 0o755)


def _run(venvs, fr_ok, extra_env=None):
    """Run the real script with a fake HOME/pipx tree.

    Returns (rc, stdout, elapsed) where elapsed is the wall time of the script
    subprocess.
    """
    with tempfile.TemporaryDirectory() as home:
        _make_tree(home, venvs)
        env = dict(os.environ)
        env["HOME"] = home
        env["FR_OK_VENVS"] = ":".join(fr_ok)
        env.pop("PV_FACE_REEXECED", None)
        env.pop("XDG_DATA_HOME", None)  # the face-deps venv must come from the fake HOME
        env.pop("FR_HANG_VENVS", None)
        env.update(extra_env or {})
        # If face_recognition happens to be importable in the test runner's
        # own interpreter, the script returns before touching pipx at all and
        # these tests are moot -- skip loudly rather than pass vacuously.
        probe = subprocess.run(
            [sys.executable, "-c", "import face_recognition"],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        )
        if probe.returncode == 0:
            raise RuntimeError("face_recognition importable in test runner; cannot test re-exec")
        started = time.monotonic()
        proc = subprocess.run(
            [sys.executable, SCRIPT, "--check"],
            env=env, capture_output=True, text=True, timeout=30,
        )
        elapsed = time.monotonic() - started
        global _last_stderr
        _last_stderr = proc.stderr
        return proc.returncode, proc.stdout, elapsed


# stderr of the most recent _run, for the tests that check the error text.
_last_stderr = ""


def _reexec_target(stdout):
    for line in stdout.splitlines():
        if line.startswith("REEXEC:"):
            return line[len("REEXEC:"):]
    return None


def test_prefers_named_face_recognition_venv():
    # face-recognition sorts AFTER ansible/black; the old code would have
    # execve'd ansible first. We must pick face-recognition regardless.
    rc, out, _ = _run(["ansible", "black", "face-recognition", "zzz"],
                      fr_ok=["face-recognition"])
    assert _reexec_target(out) == "face-recognition", out


def test_underscore_named_venv_also_matches():
    rc, out, _ = _run(["aaa", "face_recognition"], fr_ok=["face_recognition"])
    assert _reexec_target(out) == "face_recognition", out


def test_falls_back_to_probed_working_venv_when_no_named_one():
    # No venv is literally named face_recognition, but `myfaces` can import it.
    # The probe must find myfaces and skip ansible/black.
    rc, out, _ = _run(["ansible", "black", "myfaces", "zzz"], fr_ok=["myfaces"])
    assert _reexec_target(out) == "myfaces", out


def test_named_but_broken_venv_is_skipped_for_a_working_one():
    # A face-recognition venv exists but its import fails (e.g. package removed
    # from the venv); another venv actually works. Probing must skip the broken
    # named one and land on the working fallback rather than stranding us.
    rc, out, _ = _run(["face-recognition", "myfaces"], fr_ok=["myfaces"])
    assert _reexec_target(out) == "myfaces", out


def test_no_working_venv_degrades_to_unavailable_not_crash():
    # Nothing can import face_recognition. The script must NOT re-exec, and
    # must exit 2 (the "not importable" contract the Go Probe() relies on).
    rc, out, _ = _run(["ansible", "black"], fr_ok=[])
    assert _reexec_target(out) is None, out
    assert rc == 2, (rc, out)


def test_unusable_error_names_every_interpreter_and_the_fix():
    # The --check error is what Settings shows: it must give each
    # interpreter's real import error (not just "not importable from
    # /usr/bin/python3") and the one command that repairs the install.
    rc, _, _ = _run(["ansible", "face-recognition"], fr_ok=[])
    assert rc == 2, rc
    err = _last_stderr
    assert "face_recognition is not usable; tried:" in err, err
    assert sys.executable + ": No module named 'face_recognition'" in err, err
    assert "face-recognition/bin/python: ImportError: fake face-recognition" in err, err
    assert "ansible/bin/python: ImportError: fake ansible" in err, err
    assert "make face-deps" in err, err
    assert "pipx" not in err.split("Fix:")[1], err  # cfsc had no pipx


def test_face_deps_venv_wins_over_pipx():
    # The venv `make face-deps` builds is tried before any pipx venv, so it
    # works on machines without pipx and beats a stale pipx install.
    rc, out, _ = _run(["face-recognition", OWN_VENV], fr_ok=["face-recognition", OWN_VENV])
    assert _reexec_target(out) == OWN_VENV, out


def test_broken_face_deps_venv_falls_back_to_pipx():
    rc, out, _ = _run(["face-recognition", OWN_VENV], fr_ok=["face-recognition"])
    # A broken face-deps venv must not strand us: probing skips it.
    assert _reexec_target(out) == "face-recognition", out


def test_no_pipx_dir_at_all_degrades_to_unavailable():
    rc, out, _ = _run([], fr_ok=[])
    assert _reexec_target(out) is None, out
    assert rc == 2, (rc, out)


def test_wedged_interpreters_respect_total_budget():
    # Every candidate hangs on its probe and the working venv sorts last. Once
    # _PROBE_TOTAL_BUDGET is spent we must stop probing and degrade to
    # "unavailable" instead of running out the Go --check window.
    rc, out, elapsed = _run(
        ["face-recognition", "hang1", "hang2", "zzz-works"],
        fr_ok=["zzz-works"],
        extra_env={
            "FR_HANG_VENVS": "face-recognition:hang1:hang2",
            "PV_FACE_PROBE_TIMEOUT": "1",
            "PV_FACE_PROBE_BUDGET": "1.5",
        },
    )
    assert _reexec_target(out) is None, out
    assert rc == 2, (rc, out)
    assert elapsed < 4.0, elapsed


def test_per_probe_timeout_skips_wedged_to_working():
    # A wedged probe is abandoned after _PROBE_IMPORT_TIMEOUT and probing
    # continues to the working venv.
    rc, out, elapsed = _run(
        ["face-recognition", "zzz-works"],
        fr_ok=["zzz-works"],
        extra_env={
            "FR_HANG_VENVS": "face-recognition",
            "PV_FACE_PROBE_TIMEOUT": "1",
            "PV_FACE_PROBE_BUDGET": "10",
        },
    )
    assert _reexec_target(out) == "zzz-works", out
    assert elapsed < 5.0, elapsed


def _load_quality_fn():
    """Pull _face_quality out of the script without importing it (importing
    would run the pipx re-exec and the face_recognition import)."""
    import ast

    with open(SCRIPT) as fh:
        tree = ast.parse(fh.read())
    fn = next(n for n in tree.body if isinstance(n, ast.FunctionDef) and n.name == "_face_quality")
    ns = {}
    exec(compile(ast.Module(body=[fn], type_ignores=[]), SCRIPT, "exec"), ns)
    return ns["_face_quality"]


def test_face_quality_size_and_sharpness():
    try:
        import numpy as np  # type: ignore
    except ImportError:
        print("SKIP test_face_quality_size_and_sharpness: numpy not installed")
        return
    q = _load_quality_fn()
    rng = np.random.default_rng(1)
    sharp = rng.integers(0, 255, size=(200, 200, 3)).astype(np.uint8)
    flat = np.full((200, 200, 3), 128, dtype=np.uint8)
    big_sharp = q(sharp, 10, 10, 80, 80)
    big_flat = q(flat, 10, 10, 80, 80)
    tiny_sharp = q(sharp, 10, 10, 10, 10)
    assert 0.99 <= big_sharp <= 1.0, big_sharp
    assert abs(big_flat - 0.5) < 1e-6, big_flat  # full size, zero sharpness
    assert tiny_sharp == 0.0, tiny_sharp  # under 12 px scores 0
    ramp = np.tile(np.arange(200, dtype=np.uint8)[None, :, None], (200, 1, 3))
    assert q(ramp, 10, 10, 80, 80) < big_sharp  # smooth (blurry) face scores lower
    mid = q(sharp, 10, 10, 36, 36)
    assert tiny_sharp < mid < big_sharp, mid  # grows with face size
    assert q(sharp, 500, 500, 40, 40) == 0.0  # entirely outside the image


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
