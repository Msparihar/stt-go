"""Benchmark mlx-whisper models on a real stt-go recording.

Usage: .venv/bin/python bench.py <wav-file>
Prints load time, median warm inference time, and transcript per model.
"""
import json
import statistics
import sys
import time
import wave

import numpy as np
import mlx_whisper

MODELS = [
    "mlx-community/whisper-large-v3-turbo",
    "mlx-community/distil-whisper-large-v3",
    "mlx-community/whisper-small-mlx",
    "mlx-community/whisper-base-mlx",
]

WARM_RUNS = 3


def load_wav(path):
    with wave.open(path) as w:
        frames = w.readframes(w.getnframes())
        audio = np.frombuffer(frames, dtype=np.int16).astype(np.float32) / 32768.0
        if w.getnchannels() > 1:
            audio = audio.reshape(-1, w.getnchannels()).mean(axis=1)
        return audio, w.getframerate()


def main():
    wav_path = sys.argv[1]
    audio, rate = load_wav(wav_path)
    dur = len(audio) / rate
    print(f"audio: {wav_path} ({dur:.1f}s @ {rate}Hz)", flush=True)

    results = []
    for repo in MODELS:
        try:
            t0 = time.time()
            r = mlx_whisper.transcribe(audio, path_or_hf_repo=repo, language="en")
            t_first = time.time() - t0

            times = []
            for _ in range(WARM_RUNS):
                t0 = time.time()
                r = mlx_whisper.transcribe(audio, path_or_hf_repo=repo, language="en")
                times.append(time.time() - t0)
            warm = statistics.median(times)
            text = r["text"].strip()
            results.append({"model": repo, "first_sec": round(t_first, 2),
                            "warm_sec": round(warm, 3), "rtf": round(dur / warm, 1),
                            "text": text})
            print(f"DONE {repo}: first={t_first:.2f}s warm={warm:.3f}s "
                  f"({dur/warm:.1f}x realtime)\n  text: {text}", flush=True)
        except Exception as exc:
            results.append({"model": repo, "error": str(exc)[:200]})
            print(f"FAIL {repo}: {exc}", flush=True)

    print("\nJSON:" + json.dumps(results), flush=True)


if __name__ == "__main__":
    main()
