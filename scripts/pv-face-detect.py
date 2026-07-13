#!/usr/bin/env python3
# Usage:
#   pv-face-detect <image-path>   # one-shot; stdout is a JSON list of detections
#   pv-face-detect --check        # smoke-test the install; stdout starts with "ok "
#   pv-face-detect --server       # long-running NDJSON request/response loop
#
# Server mode protocol (one JSON object per line, both directions):
#   request:  {"path": "/abs/path/to/image.jpg"}
#   response: {"detections": [{"bbox":[x,y,w,h],"embedding":[128 floats]}, ...]}
#       or:   {"error": "human-readable message"}
# Exit 0 on stdin EOF.
#
# Exit 0 (one-shot mode) even when no faces are found. Non-zero only on error.
import json
import os
import subprocess
import sys

# Upper bound on how long a single `python -c "import face_recognition"` probe
# may run before we treat that interpreter as unusable and move on. Kept well
# under the Go side's 15s `--check` window (see internal/face/detector.go) so a
# probe can't eat the whole budget, yet generous enough for a real (cold) dlib
# import, which is a few seconds. A wedged interpreter is skipped, not hung on.
_PROBE_IMPORT_TIMEOUT = 12.0


def _normalize_venv_name(name: str) -> str:
    """Collapse a pipx venv directory name to a comparison key so that
    `face-recognition`, `face_recognition`, `Face_Recognition`, ... all match.
    """
    return name.lower().replace("-", "").replace("_", "")


def _pipx_venv_pythons(bases: "list[str]") -> "list[str]":
    """Return candidate pipx-venv python interpreter paths, ordered so the
    venv whose name looks like `face_recognition`/`face-recognition` comes
    first, with every other discovered venv following in sorted order.

    Pure aside from filesystem reads (`isdir`/`listdir`/`exists`), which makes
    it straightforward to exercise against a temp fake pipx tree in a test.
    """
    named: "list[str]" = []
    others: "list[str]" = []
    for base in bases:
        if not os.path.isdir(base):
            continue
        for name in sorted(os.listdir(base)):
            py = os.path.join(base, name, "bin", "python")
            if not os.path.exists(py):
                continue
            if _normalize_venv_name(name) == "facerecognition":
                named.append(py)
            else:
                others.append(py)
    return named + others


def _can_import_face_recognition(py: str) -> bool:
    """True iff interpreter `py` can `import face_recognition` without raising
    ImportError. Runs the import in a throwaway child so we never mutate — or
    hang — this process. A crash, non-zero exit, or timeout counts as "no".

    Note: face_recognition's __init__ may `sys.exit(0)` when its model data is
    missing, so a returncode of 0 means "the package is importable", not "the
    install is healthy". The full model self-test lives in `--check`, which
    runs after we re-exec into the interpreter this selects.
    """
    try:
        proc = subprocess.run(
            [py, "-c", "import face_recognition"],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            timeout=_PROBE_IMPORT_TIMEOUT,
        )
    except (OSError, subprocess.SubprocessError):
        return False
    return proc.returncode == 0


def _try_reexec_with_pipx() -> None:
    """If face_recognition isn't importable here, look for it inside common
    pipx venvs and re-exec the script under an interpreter that actually has
    it. This lets `pipx install face_recognition` work without forcing the
    user to duplicate the package into the system Python.

    Selection is deliberately careful: `os.execve` *replaces* this process, so
    execing into the wrong venv (e.g. an alphabetically-earlier `ansible` or
    `black`) would strand us in an interpreter where the import fails and the
    whole helper reports "broken" even though a working venv exists. We first
    prefer a venv literally named face_recognition, then fall back to any other
    venv, and — crucially — PROBE each candidate with a throwaway
    `import face_recognition` before committing to it, only re-exec'ing into
    one whose probe succeeds. If none qualifies we return and let the normal
    import path below degrade to "unavailable".
    """
    if os.environ.get("PV_FACE_REEXECED") == "1":
        return
    try:
        import face_recognition  # noqa: F401
        return
    except ImportError:
        pass
    home = os.path.expanduser("~")
    bases = [
        os.path.join(home, ".local", "pipx", "venvs"),
        os.path.join(home, ".local", "share", "pipx", "venvs"),
    ]
    for py in _pipx_venv_pythons(bases):
        if not _can_import_face_recognition(py):
            continue
        env = os.environ.copy()
        env["PV_FACE_REEXECED"] = "1"
        try:
            os.execve(py, [py] + sys.argv, env)
        except OSError:
            continue


_try_reexec_with_pipx()

# face_recognition's __init__ calls sys.exit(0) when its model files aren't
# importable — the import "succeeds" silently from the OS's point of view.
# Catch SystemExit explicitly so a broken install can't masquerade as good.
try:
    import face_recognition  # type: ignore
except ImportError:
    sys.stderr.write(
        "face_recognition not importable from "
        + sys.executable
        + ". Install it on this interpreter, e.g.\n"
        + "  pip install --user face_recognition\n"
        + "or expose your pipx venv to PATH.\n"
    )
    sys.exit(2)
except SystemExit as e:
    sys.stderr.write(
        "face_recognition import aborted on "
        + sys.executable
        + f" (SystemExit code={e.code}). "
        + "This usually means face_recognition_models can't load its data files — "
        + "in a pipx venv on Python 3.12+ that's most often a missing setuptools.\n"
        + "Try: pipx inject face-recognition setuptools\n"
        + "Or:  pip install --user setuptools face_recognition\n"
    )
    sys.exit(2)


def _detect_one(path: str):
    img = face_recognition.load_image_file(path)
    locs = face_recognition.face_locations(img, model="hog")
    encs = face_recognition.face_encodings(img, locs) if locs else []
    out = []
    for (top, right, bottom, left), enc in zip(locs, encs):
        out.append(
            {
                "bbox": [int(left), int(top), int(right - left), int(bottom - top)],
                "embedding": [float(x) for x in enc],
            }
        )
    return out


def _serve() -> int:
    # Read one JSON request per line, write one JSON response per line.
    # Each response includes either "detections" or "error". Faulty individual
    # requests do not terminate the server.
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            req = json.loads(line)
            path = req["path"]
        except Exception as e:  # noqa: BLE001
            sys.stdout.write(json.dumps({"error": f"bad request: {e}"}) + "\n")
            sys.stdout.flush()
            continue
        try:
            dets = _detect_one(path)
            sys.stdout.write(json.dumps({"detections": dets}) + "\n")
        except Exception as e:  # noqa: BLE001
            sys.stdout.write(json.dumps({"error": f"{type(e).__name__}: {e}"}) + "\n")
        sys.stdout.flush()
    return 0


def main() -> int:
    if len(sys.argv) != 2:
        sys.stderr.write("usage: pv-face-detect <image-path>|--check|--server\n")
        return 64
    if sys.argv[1] == "--server":
        return _serve()
    if sys.argv[1] == "--check":
        # End-to-end smoke test: ask face_recognition to scan a tiny
        # solid-grey buffer. This actually loads the dlib models, which is
        # the failure mode we care about (face_recognition's import alone
        # only prints a warning and keeps going when models are missing,
        # so an import-only check passes a half-broken install).
        try:
            import numpy as np  # type: ignore
            buf = np.zeros((32, 32, 3), dtype=np.uint8)
            face_recognition.face_locations(buf, model="hog")
        except Exception as e:  # noqa: BLE001 - any failure here = broken
            sys.stderr.write(
                f"detection self-test failed on {sys.executable}: "
                + f"{type(e).__name__}: {e}\n"
            )
            return 3
        sys.stdout.write("ok " + sys.executable)
        return 0
    path = sys.argv[1]
    try:
        out = _detect_one(path)
    except Exception as e:  # noqa: BLE001 - report any decode failure
        sys.stderr.write(f"load {path}: {e}\n")
        return 1
    json.dump(out, sys.stdout)
    return 0


if __name__ == "__main__":
    sys.exit(main())
