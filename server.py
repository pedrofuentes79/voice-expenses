import asyncio
import json
import os
import time
from datetime import datetime
from pathlib import Path
from typing import Literal
from zoneinfo import ZoneInfo

from dotenv import load_dotenv
from fastapi import FastAPI, UploadFile
from fastapi.staticfiles import StaticFiles
from openai import AsyncOpenAI
from pydantic import BaseModel, Field

load_dotenv()

try:
    import local_whisper
except ImportError:
    local_whisper = None

TZ = ZoneInfo(os.environ.get("APP_TZ", "UTC"))
DATA_DIR = Path(__file__).parent / "data" / "recordings"
TRANSCRIBE_MODEL = "gpt-4o-mini-transcribe"
PARSE_MODEL = "gpt-5-nano"

# Generated from a Kesef backup by import_kesef.py.
CONFIG = json.loads((Path(__file__).parent / "config.json").read_text())
EXPENSE_CATEGORIES: list[str] = CONFIG["expense_categories"]
INCOME_CATEGORIES: list[str] = CONFIG["income_categories"]
PAYMENT_METHODS: list[str] = CONFIG["payment_methods"]
ExpenseCategory = Literal[tuple(EXPENSE_CATEGORIES)]  # pyright: ignore[reportInvalidTypeForm]
IncomeCategory = Literal[tuple(INCOME_CATEGORIES)]  # pyright: ignore[reportInvalidTypeForm]
PaymentMethod = Literal[tuple(PAYMENT_METHODS)]  # pyright: ignore[reportInvalidTypeForm]

TERMS_FILE = Path(__file__).parent / "terms.txt"

# Context, not vocabulary: tells the transcriber what kind of speech to expect.
TRANSCRIBE_PROMPT = (
    "Audio en español rioplatense registrando gastos o ingresos: montos, "
    "nombres de comercios y restaurantes, nombres de personas, medios de pago."
)


def describe(categories: list[str]) -> str:
    descriptions = CONFIG.get("category_descriptions", {})
    return "\n".join(
        f"  - {c}: {descriptions[c]}" if c in descriptions else f"  - {c}" for c in categories
    )


EXAMPLES = "\n".join(
    f'- "{e["note"]}" → {e["kind"]}, {e["category"]}, {e["payment_method"]}, {e["amount"]:g} {e["currency"]}'
    for e in CONFIG["examples"]
)

PARSE_INSTRUCTIONS = f"""\
Convertí la transcripción de un mensaje de voz en movimientos de dinero.

- Un mensaje puede tener varios movimientos; devolvé uno por cada gasto o ingreso.
- Si no hay ningún monto, devolvé una lista vacía.
- Montos en jerga argentina: "lucas"/"luquitas" = miles, "palo" = millón, "k" = miles, \
"mangos"/"pesos" = ARS. "70 lucas" = 70000.
- Moneda ARS salvo que diga dólares/USD/verdes.
- Categorías de gasto:
{describe(EXPENSE_CATEGORIES)}
- Categorías de ingreso:
{describe(INCOME_CATEGORIES)}
- Si ninguna categoría encaja con claridad, category = null y el usuario la elige.
- Medios de pago: {", ".join(PAYMENT_METHODS)}. "efectivo" = Cash. La transcripción puede \
deformar el nombre: elegí el más parecido. null si no se menciona.
- La transcripción puede tener errores en nombres propios: copiá el nombre del comercio \
tal cual se transcribió, no lo inventes ni lo corrijas.
- "merchant": el comercio o lugar donde se gastó, nunca una persona. null si no se menciona.
- "note": descripción corta en español, incluyendo con quién fue si se menciona.
- Fecha: hoy salvo que diga "ayer", un día de la semana, etc. Formato YYYY-MM-DD.

Movimientos que el usuario ya cargó, para que imites cómo categoriza:
{EXAMPLES}
"""


def transcribe_prompt() -> str:
    """Re-read terms.txt on every request so edits apply without a restart."""
    terms = [t.strip() for t in TERMS_FILE.read_text().splitlines()] if TERMS_FILE.exists() else []
    terms = [t for t in terms if t and not t.startswith("#")]
    terms += [m for m in PAYMENT_METHODS if m not in terms]
    return TRANSCRIBE_PROMPT + f" Nombres frecuentes: {', '.join(terms)}."


class _Entry(BaseModel):
    amount: float = Field(gt=0)
    currency: Literal["ARS", "USD"]
    merchant: str | None
    payment_method: PaymentMethod | None
    note: str
    date: str


# Split by kind so the schema itself rules out an expense with an income category.
class Expense(_Entry):
    kind: Literal["expense"]
    category: ExpenseCategory | None


class Income(_Entry):
    kind: Literal["income"]
    category: IncomeCategory | None


Entry = Expense | Income  # anyOf: OpenAI strict schemas reject a discriminator's oneOf


class Parsed(BaseModel):
    entries: list[Entry]


client = AsyncOpenAI()
app = FastAPI()


async def parse(transcript: str, now: datetime) -> list[dict]:
    if not transcript.strip():
        return []
    response = await client.responses.parse(
        model=PARSE_MODEL,
        instructions=PARSE_INSTRUCTIONS,
        input=f"Hoy es {now:%A %Y-%m-%d}.\n\nTranscripción: {transcript}",
        text_format=Parsed,
        reasoning={"effort": "minimal"},
    )
    parsed = response.output_parsed
    return [e.model_dump() for e in parsed.entries] if parsed else []


async def run_engine(engine: str, data: bytes, ext: str, content_type: str, now: datetime) -> dict:
    t0 = time.perf_counter()
    try:
        if engine == "openai":
            transcription = await client.audio.transcriptions.create(
                model=TRANSCRIBE_MODEL,
                file=(f"audio.{ext}", data, content_type),
                language="es",
                prompt=transcribe_prompt(),
            )
            transcript = transcription.text
        else:
            assert local_whisper is not None
            transcript = await asyncio.to_thread(local_whisper.transcribe, data, transcribe_prompt())
        t1 = time.perf_counter()
        entries = await parse(transcript, now)
    except Exception as exc:  # noqa: BLE001 - one engine failing must not hide the other
        return {"engine": engine, "error": repr(exc)}
    t2 = time.perf_counter()
    return {
        "engine": engine,
        "transcript": transcript,
        "entries": entries,
        "timings_ms": {"transcribe": round((t1 - t0) * 1000), "parse": round((t2 - t1) * 1000)},
    }


@app.post("/api/voice")
async def voice(audio: UploadFile) -> dict:
    data = await audio.read()
    now = datetime.now(TZ)
    # Safari records audio/mp4; OpenAI picks the decoder from the filename extension.
    content_type = audio.content_type or "audio/mp4"
    ext = "m4a" if "mp4" in content_type else "webm"
    stem = now.strftime("%Y%m%d-%H%M%S")
    DATA_DIR.mkdir(parents=True, exist_ok=True)
    (DATA_DIR / f"{stem}.{ext}").write_bytes(data)

    engines = ["openai"] + ([f"whisper-{local_whisper.MODEL}"] if local_whisper else [])
    results = await asyncio.gather(*(run_engine(e, data, ext, content_type, now) for e in engines))
    result = {"results": results}
    (DATA_DIR / f"{stem}.json").write_text(json.dumps(result, ensure_ascii=False, indent=2))
    return result


app.mount("/", StaticFiles(directory=Path(__file__).parent / "speech-test", html=True), name="static")
