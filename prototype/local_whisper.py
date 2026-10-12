"""Local faster-whisper for testing on a CUDA desktop; the Pi never imports this."""

import ctypes
import subprocess
import tempfile
from pathlib import Path

import nvidia

# ctranslate2 dlopens cuBLAS/cuDNN by soname; the pip wheels aren't on the loader path.
_NVIDIA = Path(nvidia.__path__[0])
for _lib in ("cublas/lib/libcublasLt.so.12", "cublas/lib/libcublas.so.12", "cudnn/lib/libcudnn.so.9"):
    ctypes.CDLL(str(_NVIDIA / _lib), mode=ctypes.RTLD_GLOBAL)

import numpy as np
from faster_whisper import WhisperModel

MODEL = "large-v3-turbo"
_model: WhisperModel | None = None


def transcribe(data: bytes, prompt: str) -> str:
    global _model
    if _model is None:
        _model = WhisperModel(MODEL, device="cuda", compute_type="int8_float16")
    # faster-whisper's PyAV decoder breaks on av>=17. ffmpeg needs a real file: Safari's
    # mp4 puts the moov atom last, so it can't be demuxed from a pipe.
    with tempfile.NamedTemporaryFile(suffix=".m4a") as f:
        f.write(data)
        f.flush()
        cmd = [
            "ffmpeg",
            "-loglevel",
            "error",
            "-i",
            f.name,
            "-f",
            "f32le",
            "-ac",
            "1",
            "-ar",
            "16000",
            "-",
        ]
        pcm = subprocess.run(cmd, capture_output=True, check=True).stdout
    segments, _ = _model.transcribe(
        np.frombuffer(pcm, dtype=np.float32), language="es", initial_prompt=prompt, vad_filter=True
    )
    return " ".join(s.text.strip() for s in segments)
