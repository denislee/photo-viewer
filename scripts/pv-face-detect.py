#!/usr/bin/env python3
# Usage:
#   pv-face-detect <image-path>   # one-shot; stdout is a JSON list of detections
#   pv-face-detect --check        # smoke-test the install; stdout starts with "ok "
#   pv-face-detect --server       # long-running NDJSON request/response loop
#
# Server mode protocol (one JSON object per line, both directions):
#   request:  {"path": "/abs/path/to/image.jpg"}
#   response: {"detections": [{"bbox":[x,y,w,h],"embedding":[128 floats],
#                              "quality":0..1}, ...], "size":[img_w,img_h]}
#       or:   {"error": "human-readable message"}
# Exit 0 on stdin EOF.
#
# Exit 0 (one-shot mode) even when no faces are found. Non-zero only on error.
import json
import os
import subprocess
import sys
import time
import warnings

# face_recognition_models imports pkg_resources, which prints a deprecation
# UserWarning on every start. The Go side shows this process's stderr as the
# helper status in Settings, so keep it to real errors.
warnings.filterwarnings("ignore", message=".*pkg_resources is deprecated.*")

# One "<interpreter>: <why>" line per interpreter that could not import
# face_recognition, so a failed --check names the real cause (a missing
# module, or e.g. a dlib built against a since-upgraded shared library)
# instead of only the last interpreter tried.
_import_failures: "list[str]" = []

# The one-line fix printed with every "not usable" error. `make face-deps`
# (re)builds a private venv with face_recognition and a pkg_resources-providing
# setuptools, using only python3's stdlib venv (no pipx needed).
_FIX_HINT = (
    "Fix: run `make face-deps` in the photo-viewer checkout (builds dlib into "
    "a private venv; needs cmake and a C++ compiler)"
)


def _own_venv_python() -> str:
    """The interpreter of the venv `make face-deps` builds:
    $XDG_DATA_HOME/photo-viewer/face-venv (default ~/.local/share/...)."""
    data = os.environ.get("XDG_DATA_HOME") or os.path.join(
        os.path.expanduser("~"), ".local", "share"
    )
    return os.path.join(data, "photo-viewer", "face-venv", "bin", "python")


def _last_line(text: str) -> str:
    lines = [l.strip() for l in text.strip().splitlines() if l.strip()]
    return lines[-1] if lines else ""

# Probe budgets for picking a pipx venv (S3-05). A single
# `python -c "import face_recognition"` probe may run for at most
# _PROBE_IMPORT_TIMEOUT seconds (a cold dlib import is a few seconds), and all
# probes together for at most _PROBE_TOTAL_BUDGET, after which we stop looking.
# The total keeps a run of wedged interpreters (NFS home, cold antivirus,
# half-installed venv) from eating the Go side's 15 s `--check` window (see
# internal/face/detector.go) and leaving no time for the re-exec'd self-test.
# The PV_FACE_PROBE_TIMEOUT / PV_FACE_PROBE_BUDGET env overrides exist for
# scripts/test_pv_face_detect.py.
def _env_seconds(name: str, default: float) -> float:
    try:
        return float(os.environ.get(name, default))
    except ValueError:
        return default


_PROBE_IMPORT_TIMEOUT = _env_seconds("PV_FACE_PROBE_TIMEOUT", 4.0)
_PROBE_TOTAL_BUDGET = _env_seconds("PV_FACE_PROBE_BUDGET", 12.0)


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


def _can_import_face_recognition(py: str, timeout: float = _PROBE_IMPORT_TIMEOUT) -> bool:
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
            env={**os.environ, "PYTHONWARNINGS": "ignore"},
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.PIPE,
            text=True,
            timeout=timeout,
        )
    except subprocess.TimeoutExpired:
        _import_failures.append(f"{py}: import timed out after {timeout:g}s")
        return False
    except (OSError, subprocess.SubprocessError) as e:
        _import_failures.append(f"{py}: {e}")
        return False
    if proc.returncode != 0:
        why = _last_line(proc.stderr) or f"exit status {proc.returncode}"
        _import_failures.append(f"{py}: {why}")
        return False
    return True


def _try_reexec_with_venv() -> None:
    """If face_recognition isn't importable here, look for it in the venv
    `make face-deps` builds, then in common pipx venvs, and re-exec the script
    under an interpreter that actually has it, so it never has to be
    installed into the system Python.

    Selection is deliberately careful: `os.execve` *replaces* this process, so
    execing into the wrong venv (e.g. an alphabetically-earlier `ansible` or
    `black`) would strand us in an interpreter where the import fails and the
    whole helper reports "broken" even though a working venv exists. We first
    prefer a venv literally named face_recognition, then fall back to any other
    venv, and — crucially — PROBE each candidate with a throwaway
    `import face_recognition` before committing to it, only re-exec'ing into
    one whose probe succeeds. If none qualifies we return and let the normal
    import path below degrade to "unavailable".

    Probing stops once _PROBE_TOTAL_BUDGET is spent.
    """
    if os.environ.get("PV_FACE_REEXECED") == "1":
        return
    try:
        import face_recognition  # noqa: F401
        return
    except ImportError as e:
        _import_failures.append(f"{sys.executable}: {e}")
    home = os.path.expanduser("~")
    bases = [
        os.path.join(home, ".local", "pipx", "venvs"),
        os.path.join(home, ".local", "share", "pipx", "venvs"),
    ]
    deadline = time.monotonic() + _PROBE_TOTAL_BUDGET
    own = _own_venv_python()
    candidates = ([own] if os.path.exists(own) else []) + _pipx_venv_pythons(bases)
    for py in candidates:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            # Budget spent; degrade to "unavailable" rather than time out the
            # Go --check.
            return
        if not _can_import_face_recognition(py, min(_PROBE_IMPORT_TIMEOUT, remaining)):
            continue
        env = os.environ.copy()
        env["PV_FACE_REEXECED"] = "1"
        try:
            os.execve(py, [py] + sys.argv, env)
        except OSError:
            continue


_try_reexec_with_venv()

# face_recognition's __init__ calls sys.exit(0) when its model files aren't
# importable — the import "succeeds" silently from the OS's point of view.
# Catch SystemExit explicitly so a broken install can't masquerade as good.
try:
    import face_recognition  # type: ignore
except ImportError as e:
    if not _import_failures:  # re-exec'd, or the pipx pass never ran
        _import_failures.append(f"{sys.executable}: {e}")
    sys.stderr.write(
        "face_recognition is not usable; tried:\n"
        + "".join(f"  {f}\n" for f in _import_failures)
        + _FIX_HINT
        + "\n"
    )
    sys.exit(2)
except SystemExit as e:
    sys.stderr.write(
        "face_recognition import aborted on "
        + sys.executable
        + f" (SystemExit code={e.code}): face_recognition_models can't load "
        + "its data files, most often a missing setuptools (pkg_resources).\n"
        + _FIX_HINT
        + "\n"
    )
    sys.exit(2)


def _face_quality(img, left: int, top: int, width: int, height: int) -> float:
    """Detector-side quality score in 0..1: face size times sharpness.

    size:  0 at a 12 px face, 1 at 60 px and up (HOG's embeddings are noisy
           on tiny crops, which is the main source of wrong groups).
    sharp: variance of a 4-neighbour Laplacian over the grey face crop,
           mapped so ~150 (a crisp face) is 1; motion blur and heavy JPEG
           smoothing push it toward 0.
    The score is size * (0.5 + 0.5 * sharp), so a big blurry face still
    outranks a tiny sharp one. Pure numpy so it can be unit-tested without
    face_recognition.
    """
    import numpy as np  # type: ignore

    side = min(width, height)
    size = min(max((side - 12) / 48.0, 0.0), 1.0)
    h, w = img.shape[0], img.shape[1]
    x0, y0 = max(left, 0), max(top, 0)
    x1, y1 = min(left + width, w), min(top + height, h)
    if x1 - x0 < 3 or y1 - y0 < 3:
        return 0.0
    crop = np.asarray(img[y0:y1, x0:x1], dtype=np.float64)
    grey = crop.mean(axis=2) if crop.ndim == 3 else crop
    lap = (
        4 * grey[1:-1, 1:-1]
        - grey[:-2, 1:-1]
        - grey[2:, 1:-1]
        - grey[1:-1, :-2]
        - grey[1:-1, 2:]
    )
    sharp = min(float(lap.var()) / 150.0, 1.0)
    return round(size * (0.5 + 0.5 * sharp), 4)


def _detect_one(path: str):
    img = face_recognition.load_image_file(path)
    locs = face_recognition.face_locations(img, model="hog")
    encs = face_recognition.face_encodings(img, locs) if locs else []
    out = []
    for (top, right, bottom, left), enc in zip(locs, encs):
        w, h = int(right - left), int(bottom - top)
        out.append(
            {
                "bbox": [int(left), int(top), w, h],
                "embedding": [float(x) for x in enc],
                "quality": _face_quality(img, int(left), int(top), w, h),
            }
        )
    return out, [int(img.shape[1]), int(img.shape[0])]


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
            dets, size = _detect_one(path)
            sys.stdout.write(json.dumps({"detections": dets, "size": size}) + "\n")
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
        out, _ = _detect_one(path)
    except Exception as e:  # noqa: BLE001 - report any decode failure
        sys.stderr.write(f"load {path}: {e}\n")
        return 1
    json.dump(out, sys.stdout)
    return 0


if __name__ == "__main__":
    sys.exit(main())
