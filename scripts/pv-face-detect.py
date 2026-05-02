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
import sys


def _try_reexec_with_pipx() -> None:
    """If face_recognition isn't importable here, look for it inside common
    pipx venvs and re-exec the script under that interpreter. This lets
    `pipx install face_recognition` work without forcing the user to
    duplicate the package into the system Python.
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
    candidates: list[str] = []
    for base in bases:
        if not os.path.isdir(base):
            continue
        for name in sorted(os.listdir(base)):
            py = os.path.join(base, name, "bin", "python")
            if os.path.exists(py):
                candidates.append(py)
    for py in candidates:
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
