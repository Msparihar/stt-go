r"""Local faster-whisper sidecar for STT-Go.

The process is lightweight at rest: the model is NOT loaded on startup. It loads
only when STT-Go switches to the whisper_local backend (POST /load, or lazily on
the first /transcribe) and is freed when STT-Go switches away (POST /unload). So
the ~1.4GB of VRAM is occupied only while the local backend is actually selected.

POST /load            load the model into VRAM (idempotent)
POST /unload          free the model from VRAM (idempotent)
POST /transcribe      WAV body, headers X-Language / X-Prompt; auto-loads if idle
GET  /health          { loaded: bool, model, compute }

Run:  <venv>/Scripts/pythonw.exe sidecar/server.py
Env:  WHISPER_MODEL (default large-v3-turbo), WHISPER_COMPUTE (float16),
      WHISPER_PORT (5111), WHISPER_MODEL_DIR (default: sidecar/models/)
"""
import io
import os
import sys
import gc
import json
import time
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

# Default model cache next to this script so the exe dir stays clean.
_here = os.path.dirname(os.path.abspath(__file__))
_default_model_dir = os.path.join(_here, "models")
os.environ.setdefault("HF_HOME", os.environ.get("WHISPER_MODEL_DIR", _default_model_dir))

# cuBLAS via add_dll_directory; cuDNN sub-DLLs are LoadLibrary'd at runtime and
# only honor PATH, so prepend both bin dirs there too.
_site = os.path.join(os.path.dirname(sys.executable), "..", "Lib", "site-packages", "nvidia")
for _sub in ("cublas", "cudnn"):
    _dir = os.path.abspath(os.path.join(_site, _sub, "bin"))
    if os.path.isdir(_dir):
        os.add_dll_directory(_dir)
        os.environ["PATH"] = _dir + os.pathsep + os.environ.get("PATH", "")

from faster_whisper import WhisperModel

MODEL_SIZE = os.environ.get("WHISPER_MODEL", "large-v3-turbo")  # proven default; large-v3 still under eval
COMPUTE = os.environ.get("WHISPER_COMPUTE", "float16")
PORT = int(os.environ.get("WHISPER_PORT", "5111"))
MODEL_DIR = os.environ.get("WHISPER_MODEL_DIR", _default_model_dir)

# Guarded by _lock; None means "not in VRAM".
_model = None
_lock = threading.Lock()


def ensure_loaded():
    global _model
    with _lock:
        if _model is None:
            t0 = time.time()
            print(f"[server] loading {MODEL_SIZE} (compute={COMPUTE}) on GPU...", flush=True)
            _model = WhisperModel(MODEL_SIZE, device="cuda", compute_type=COMPUTE,
                                  download_root=MODEL_DIR)
            warm = os.path.join(_here, "test.wav")
            if os.path.isfile(warm):
                list(_model.transcribe(warm, beam_size=1)[0])
            print(f"[server] model ready in {time.time() - t0:.1f}s", flush=True)
        return _model


def unload():
    global _model
    with _lock:
        if _model is not None:
            _model = None
            gc.collect()
            # CTranslate2 releases its CUDA allocations when the model is GC'd;
            # gc.collect() forces that promptly so VRAM frees right away.
            print("[server] model unloaded, VRAM freed", flush=True)


class Handler(BaseHTTPRequestHandler):
    def _json(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path == "/health":
            self._json(200, {"loaded": _model is not None, "model": MODEL_SIZE, "compute": COMPUTE})
        else:
            self._json(404, {"error": "not found"})

    def do_POST(self):
        if self.path == "/load":
            ensure_loaded()
            self._json(200, {"loaded": True, "model": MODEL_SIZE})
            return
        if self.path == "/unload":
            unload()
            self._json(200, {"loaded": False})
            return
        if self.path != "/transcribe":
            self._json(404, {"error": "not found"})
            return

        length = int(self.headers.get("Content-Length", "0"))
        if length <= 0:
            self._json(400, {"error": "empty body"})
            return
        wav = self.rfile.read(length)
        lang = self.headers.get("X-Language", "en") or None
        prompt = self.headers.get("X-Prompt", "") or None
        try:
            model = ensure_loaded()  # lazy-load if STT-Go didn't pre-warm
            t0 = time.time()
            segments, info = model.transcribe(
                io.BytesIO(wav), beam_size=5, language=lang, initial_prompt=prompt,
            )
            text = "".join(s.text for s in segments).strip()
            infer = time.time() - t0
        except Exception as exc:  # noqa: BLE001 — report any decode/inference failure to the client
            self._json(500, {"error": str(exc)})
            return
        print(f"[server] {info.duration:.1f}s audio -> {infer:.2f}s "
              f"({info.duration / infer:.1f}x) : {text[:60]}", flush=True)
        self._json(200, {"text": text, "duration": info.duration, "infer_sec": infer})

    def log_message(self, *args):
        pass  # silence default per-request stderr logging; we print our own


if __name__ == "__main__":
    srv = ThreadingHTTPServer(("127.0.0.1", PORT), Handler)
    print(f"[server] listening on http://127.0.0.1:{PORT} (model idle until /load)", flush=True)
    srv.serve_forever()
