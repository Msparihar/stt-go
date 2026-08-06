r"""Local Whisper sidecar for STT-Go.

Two engines, picked automatically:
  - mlx    — Apple Silicon (mlx-whisper, Metal GPU). Default on macOS.
  - faster — faster-whisper on CUDA (Windows/Linux with NVIDIA GPU).
Override with WHISPER_ENGINE=mlx|faster.

The process is lightweight at rest: the model is NOT loaded on startup. It loads
only when STT-Go switches to the whisper_local backend (POST /load, or lazily on
the first /transcribe) and is freed when STT-Go switches away (POST /unload). So
the ~1.5GB of GPU memory is occupied only while the local backend is selected.

POST /load            load the model (idempotent)
POST /unload          free the model (idempotent)
POST /transcribe      WAV body, headers X-Language / X-Prompt; auto-loads if idle
GET  /health          { loaded: bool, model, compute }

Run:  <venv python> sidecar/server.py
Env:  WHISPER_MODEL (default large-v3-turbo), WHISPER_COMPUTE (float16),
      WHISPER_PORT (5111), WHISPER_MODEL_DIR (default: sidecar/models/),
      WHISPER_ENGINE (mlx|faster, default auto)
"""
import io
import os
import sys
import gc
import json
import time
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

# Model cache: standard HF cache on macOS (shared with other tools);
# next to this script on Windows so the exe dir stays clean.
_here = os.path.dirname(os.path.abspath(__file__))
if sys.platform == "darwin":
    _default_model_dir = os.path.expanduser("~/.cache/huggingface")
    os.environ.setdefault("HF_HUB_ENABLE_HF_TRANSFER", "1")
else:
    _default_model_dir = os.path.join(_here, "models")
os.environ.setdefault("HF_HOME", os.environ.get("WHISPER_MODEL_DIR", _default_model_dir))

# Engine selection: mlx on Apple Silicon, faster-whisper (CUDA) elsewhere.
ENGINE = os.environ.get("WHISPER_ENGINE", "")
if not ENGINE:
    if sys.platform == "darwin":
        ENGINE = "mlx"
    else:
        ENGINE = "faster"

if ENGINE == "faster":
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

# MLX pulls converted weights from the mlx-community HF repos.
MLX_MODEL = os.environ.get("WHISPER_MLX_MODEL", f"mlx-community/whisper-{MODEL_SIZE}")

# Guarded by _lock; None means "not loaded".
_model = None
_lock = threading.Lock()


def _wav_to_float32(wav_bytes):
    """Decode a 16-bit PCM WAV into a mono float32 numpy array for MLX."""
    import wave
    import numpy as np

    with wave.open(io.BytesIO(wav_bytes)) as w:
        frames = w.readframes(w.getnframes())
        audio = np.frombuffer(frames, dtype=np.int16).astype(np.float32) / 32768.0
        if w.getnchannels() > 1:
            audio = audio.reshape(-1, w.getnchannels()).mean(axis=1)
        return audio


def ensure_loaded():
    global _model
    with _lock:
        if _model is None:
            t0 = time.time()
            if ENGINE == "mlx":
                print(f"[server] loading {MLX_MODEL} via MLX (Metal)...", flush=True)
                import mlx_whisper  # noqa: F401 — model weights fetched on first transcribe
                import numpy as np
                # Warm up: transcribing silence forces the weight download + compile.
                mlx_whisper.transcribe(np.zeros(16000, dtype=np.float32), path_or_hf_repo=MLX_MODEL)
                _model = "mlx"
            else:
                print(f"[server] loading {MODEL_SIZE} (compute={COMPUTE}) on GPU...", flush=True)
                _model = WhisperModel(MODEL_SIZE, device="cuda", compute_type=COMPUTE,
                                      download_root=MODEL_DIR)
                warm = os.path.join(_here, "test.wav")
                if os.path.isfile(warm):
                    list(_model.transcribe(warm, beam_size=1)[0])
            print(f"[server] model ready in {time.time() - t0:.1f}s", flush=True)
        return _model


def transcribe_wav(wav, lang, prompt):
    """Run the active engine on WAV bytes; returns (text, audio_duration_sec)."""
    if ENGINE == "mlx":
        import mlx_whisper
        audio = _wav_to_float32(wav)
        result = mlx_whisper.transcribe(
            audio, path_or_hf_repo=MLX_MODEL, language=lang, initial_prompt=prompt,
        )
        return result["text"].strip(), len(audio) / 16000.0
    model = ensure_loaded()
    segments, info = model.transcribe(
        io.BytesIO(wav), beam_size=5, language=lang, initial_prompt=prompt,
    )
    return "".join(s.text for s in segments).strip(), info.duration


def unload():
    global _model
    with _lock:
        if _model is not None:
            if ENGINE == "mlx":
                # mlx_whisper caches the model in ModelHolder; drop it there too.
                try:
                    from mlx_whisper.transcribe import ModelHolder
                    ModelHolder.model = None
                    ModelHolder.model_path = None
                except Exception:
                    pass
            _model = None
            gc.collect()
            # CTranslate2/MLX release their GPU allocations when the model is
            # GC'd; gc.collect() forces that promptly so memory frees right away.
            print("[server] model unloaded, GPU memory freed", flush=True)


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
            ensure_loaded()  # lazy-load if STT-Go didn't pre-warm
            t0 = time.time()
            text, duration = transcribe_wav(wav, lang, prompt)
            infer = time.time() - t0
        except Exception as exc:  # noqa: BLE001 — report any decode/inference failure to the client
            self._json(500, {"error": str(exc)})
            return
        print(f"[server] {duration:.1f}s audio -> {infer:.2f}s "
              f"({duration / infer:.1f}x) : {text[:60]}", flush=True)
        self._json(200, {"text": text, "duration": duration, "infer_sec": infer})

    def log_message(self, *args):
        pass  # silence default per-request stderr logging; we print our own


if __name__ == "__main__":
    srv = ThreadingHTTPServer(("127.0.0.1", PORT), Handler)
    print(f"[server] listening on http://127.0.0.1:{PORT} (model idle until /load)", flush=True)
    srv.serve_forever()
