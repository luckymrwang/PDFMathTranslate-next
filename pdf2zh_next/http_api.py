"""HTTP API for pdf2zh_next, wrapping `do_translate_async_stream`.

A thin, mostly-stateless FastAPI service intended to sit behind a business
gateway (e.g. a Go service handling WeChat auth, OSS, billing and queueing).

Flow:
    POST   /v1/translate            upload a PDF + settings  -> {"id": ...}
    GET    /v1/translate/{id}/stream live progress via SSE (Server-Sent Events)
    GET    /v1/translate/{id}        poll current status (for non-SSE clients)
    GET    /v1/translate/{id}/mono   download monolingual result PDF
    GET    /v1/translate/{id}/dual   download bilingual result PDF
    DELETE /v1/translate/{id}        cancel a running task / clean up
    GET    /health                   liveness probe

The service keeps task state in memory, so a single task's stream/poll/download
requests must reach the same worker. For multi-worker horizontal scaling, put a
queue (Redis/MQ) and shared object storage in the gateway layer and treat each
Python worker as a stateless unit.
"""

from __future__ import annotations

import asyncio
import hashlib
import json
import logging
import os
import shutil
import tempfile
import time
import uuid
from collections.abc import AsyncGenerator
from contextlib import asynccontextmanager
from dataclasses import dataclass
from dataclasses import field
from pathlib import Path
from typing import Any
from typing import Literal

from fastapi import FastAPI
from fastapi import Form
from fastapi import Header
from fastapi import HTTPException
from fastapi import UploadFile
from fastapi.responses import FileResponse
from pydantic import BaseModel
from pydantic import Field
from sse_starlette.sse import EventSourceResponse

from pdf2zh_next.config.model import SettingsModel
from pdf2zh_next.high_level import do_translate_async_stream

logger = logging.getLogger(__name__)

# --- configuration (overridable via environment) -------------------------------

WORK_DIR = Path(os.environ.get("PDF2ZH_WORK_DIR", tempfile.gettempdir()) ) / "pdf2zh_api"
MAX_CONCURRENT = int(os.environ.get("PDF2ZH_MAX_CONCURRENT", "2"))
TASK_TTL_SECONDS = int(os.environ.get("PDF2ZH_TASK_TTL", str(2 * 60 * 60)))
UPLOAD_CHUNK_SIZE = 1 << 20  # 1 MiB

_SENTINEL = object()  # marks end of an SSE stream


# --- request / response models -------------------------------------------------


class TranslateRequest(BaseModel):
    """Translation parameters sent alongside the uploaded PDF.

    `translate_engine_settings` is passed straight to `SettingsModel` and
    validated against its discriminated union, e.g.:
        {"translate_engine_type": "Google"}
        {"translate_engine_type": "OpenAI", "openai_api_key": "sk-...",
         "openai_model": "gpt-4o-mini"}
    """

    lang_in: str = Field(default="en")
    lang_out: str = Field(default="zh")
    qps: int = Field(default=4)
    pages: str | None = Field(default=None)
    no_mono: bool = Field(default=False)
    no_dual: bool = Field(default=False)
    skip_image_translation: bool = Field(default=True)
    watermark_output_mode: Literal["watermarked", "no_watermark", "both"] = Field(
        default="watermarked"
    )
    translate_engine_settings: dict[str, Any] = Field(
        default_factory=lambda: {"translate_engine_type": "Google"}
    )


# --- in-memory task store ------------------------------------------------------


@dataclass
class TaskState:
    task_id: str
    input_path: Path
    output_dir: Path
    created_at: float = field(default_factory=time.time)
    status: str = "queued"  # queued | running | finished | error | cancelled
    stage: str = ""
    progress: float = 0.0  # 0-100 overall progress
    error: str | None = None
    error_type: str | None = None
    mono_path: Path | None = None
    dual_path: Path | None = None
    total_seconds: float | None = None
    token_usage: dict[str, Any] = field(default_factory=dict)
    done: bool = False
    history: list[dict[str, Any]] = field(default_factory=list)
    subscribers: set[asyncio.Queue] = field(default_factory=set)
    worker: asyncio.Task | None = None

    def publish(self, event: dict[str, Any]) -> None:
        """Record an event and fan it out to all live SSE subscribers.

        Runs without awaits, so it is atomic on the event loop.
        """
        self.history.append(event)
        for q in self.subscribers:
            q.put_nowait(event)

    def subscribe(self) -> asyncio.Queue:
        """Return a queue pre-filled with history, then live events."""
        q: asyncio.Queue = asyncio.Queue()
        for event in self.history:
            q.put_nowait(event)
        if self.done:
            q.put_nowait(_SENTINEL)
        else:
            self.subscribers.add(q)
        return q

    def finish_stream(self) -> None:
        self.done = True
        for q in self.subscribers:
            q.put_nowait(_SENTINEL)
        self.subscribers.clear()


_tasks: dict[str, TaskState] = {}
_semaphore: asyncio.Semaphore | None = None


def _get_semaphore() -> asyncio.Semaphore:
    global _semaphore
    if _semaphore is None:
        _semaphore = asyncio.Semaphore(MAX_CONCURRENT)
    return _semaphore


# --- settings construction -----------------------------------------------------


def _build_settings(req: TranslateRequest, output_dir: Path) -> SettingsModel:
    settings = SettingsModel(
        translate_engine_settings=req.translate_engine_settings,
    )
    settings.translation.lang_in = req.lang_in
    settings.translation.lang_out = req.lang_out
    settings.translation.qps = req.qps
    settings.translation.output = str(output_dir)
    settings.pdf.pages = req.pages
    settings.pdf.no_mono = req.no_mono
    settings.pdf.no_dual = req.no_dual
    settings.pdf.skip_image_translation = req.skip_image_translation
    settings.pdf.watermark_output_mode = req.watermark_output_mode
    return settings


def _serialize_event(event: dict[str, Any], task: TaskState) -> dict[str, Any]:
    """Convert a babeldoc event into a JSON-safe dict and update task state."""
    etype = event.get("type")

    if etype in ("progress_start", "progress_update", "progress_end"):
        task.status = "running"
        task.stage = event.get("stage", task.stage)
        task.progress = float(event.get("overall_progress", task.progress))
        return {
            "type": etype,
            "stage": event.get("stage"),
            "overall_progress": event.get("overall_progress"),
            "part_index": event.get("part_index"),
            "total_parts": event.get("total_parts"),
            "stage_current": event.get("stage_current"),
            "stage_total": event.get("stage_total"),
        }

    if etype == "finish":
        result = event.get("translate_result")
        mono = getattr(result, "mono_pdf_path", None)
        dual = getattr(result, "dual_pdf_path", None)
        task.mono_path = Path(mono) if mono else None
        task.dual_path = Path(dual) if dual else None
        task.total_seconds = getattr(result, "total_seconds", None)
        task.token_usage = event.get("token_usage", {}) or {}
        task.status = "finished"
        task.progress = 100.0
        return {
            "type": "finish",
            "total_seconds": task.total_seconds,
            "mono_url": f"/v1/translate/{task.task_id}/mono" if mono else None,
            "dual_url": f"/v1/translate/{task.task_id}/dual" if dual else None,
            "token_usage": task.token_usage,
        }

    if etype == "error":
        task.status = "error"
        task.error = str(event.get("error", "Unknown error"))
        task.error_type = event.get("error_type", "UnknownError")
        return {
            "type": "error",
            "error": task.error,
            "error_type": task.error_type,
            "details": event.get("details", ""),
        }

    # Unknown event type: pass through best-effort.
    return {"type": etype or "unknown"}


async def _run_translation(task: TaskState, settings: SettingsModel) -> None:
    """Drive do_translate_async_stream and publish serialized events."""
    sem = _get_semaphore()
    async with sem:
        try:
            async for event in do_translate_async_stream(settings, task.input_path):
                task.publish(_serialize_event(event, task))
                if event.get("type") in ("finish", "error"):
                    break
        except asyncio.CancelledError:
            task.status = "cancelled"
            task.publish({"type": "error", "error": "cancelled",
                          "error_type": "Cancelled", "details": ""})
            raise
        except Exception as e:  # noqa: BLE001 - surface any failure to the client
            logger.exception("translation task %s failed", task.task_id)
            task.status = "error"
            task.error = str(e)
            task.error_type = e.__class__.__name__
            task.publish({"type": "error", "error": task.error,
                          "error_type": task.error_type, "details": ""})
        finally:
            task.finish_stream()


# --- FastAPI app ---------------------------------------------------------------


@asynccontextmanager
async def _lifespan(app: FastAPI):
    WORK_DIR.mkdir(parents=True, exist_ok=True)
    _get_semaphore()
    cleanup = asyncio.create_task(_cleanup_loop())
    try:
        yield
    finally:
        cleanup.cancel()


app = FastAPI(title="pdf2zh_next HTTP API", version="1.0.0", lifespan=_lifespan)


@app.get("/health")
async def health() -> dict[str, Any]:
    return {
        "status": "ok",
        "active_tasks": sum(1 for t in _tasks.values() if not t.done),
        "total_tasks": len(_tasks),
        "max_concurrent": MAX_CONCURRENT,
    }


async def _create_translation(
    file: UploadFile,
    data: str = Form(default="{}"),
) -> dict[str, str]:
    """Upload a PDF and start a translation task.

    `data` is a JSON string of `TranslateRequest`.
    Returns the task id for subsequent streaming / polling / download.
    """
    try:
        req = TranslateRequest.model_validate_json(data)
    except Exception as e:  # noqa: BLE001
        raise HTTPException(status_code=422, detail=f"invalid data: {e}") from e

    if not (file.filename or "").lower().endswith(".pdf"):
        raise HTTPException(status_code=400, detail="only PDF files are supported")

    task_id = uuid.uuid4().hex
    task_dir = WORK_DIR / task_id
    output_dir = task_dir / "output"
    output_dir.mkdir(parents=True, exist_ok=True)
    input_path = task_dir / "input.pdf"

    size = 0
    with input_path.open("wb") as f:
        while chunk := await file.read(UPLOAD_CHUNK_SIZE):
            size += len(chunk)
            f.write(chunk)
    if size == 0:
        shutil.rmtree(task_dir, ignore_errors=True)
        raise HTTPException(status_code=400, detail="empty file")

    try:
        settings = _build_settings(req, output_dir)
    except Exception as e:  # noqa: BLE001
        shutil.rmtree(task_dir, ignore_errors=True)
        raise HTTPException(status_code=422, detail=f"invalid settings: {e}") from e

    task = TaskState(task_id=task_id, input_path=input_path, output_dir=output_dir)
    _tasks[task_id] = task
    task.worker = asyncio.create_task(_run_translation(task, settings))

    return {"id": task_id}


_paid_submit_lock: asyncio.Lock | None = None


@app.post("/v1/pdf/inspect")
async def inspect_pdf(file: UploadFile, data: str = Form(default="{}")) -> dict[str, int]:
    """Server-side page count for pricing; never trust a client's quantity."""
    if not (file.filename or "").lower().endswith(".pdf"):
        raise HTTPException(status_code=400, detail="only PDF files are supported")
    with tempfile.NamedTemporaryFile(suffix=".pdf") as tmp:
        size = 0
        while chunk := await file.read(UPLOAD_CHUNK_SIZE):
            size += len(chunk)
            if size > 50 * 1024 * 1024:
                raise HTTPException(status_code=413, detail="PDF exceeds 50 MB")
            tmp.write(chunk)
        tmp.flush()

        def count_pages():
            import pymupdf

            try:
                with pymupdf.open(tmp.name) as document:
                    if not document.is_pdf or document.needs_pass:
                        raise ValueError("encrypted or invalid PDF")
                    pages = document.page_count
                    if not 1 <= pages <= 1000:
                        raise ValueError("PDF must contain 1 to 1000 pages")
                    return pages
            except Exception as error:
                raise HTTPException(status_code=422, detail="cannot inspect PDF") from error

        pages = await asyncio.to_thread(count_pages)
        if data != "{}":
            # Payment quotes must not sell a translation with an unavailable
            # engine. A minimal Hello health check validates credentials first.
            try:
                from pdf2zh_next.translator import get_translator

                request = TranslateRequest.model_validate_json(data)
                settings = _build_settings(request, Path(tmp.name).parent)
                await asyncio.to_thread(get_translator, settings)
            except Exception as error:
                raise HTTPException(status_code=503, detail="translation engine unavailable") from error
        return {"page_count": pages}


@app.post("/v1/translate")
async def create_translation(
    file: UploadFile,
    data: str = Form(default="{}"),
    idempotency_key: str | None = Header(default=None, alias="X-Idempotency-Key"),
) -> dict[str, str]:
    if not idempotency_key:
        return await _create_translation(file, data)
    if len(idempotency_key) > 100:
        raise HTTPException(status_code=400, detail="invalid idempotency key")
    global _paid_submit_lock
    if _paid_submit_lock is None:
        _paid_submit_lock = asyncio.Lock()
    async with _paid_submit_lock:
        key_hash = hashlib.sha256(idempotency_key.encode()).hexdigest()
        directory = WORK_DIR / "idempotency"
        directory.mkdir(parents=True, exist_ok=True, mode=0o700)
        marker = directory / (key_hash + ".json")
        if marker.exists():
            task_id = json.loads(marker.read_text())["id"]
            if task_id not in _tasks:
                # Paid jobs cannot silently run twice after a worker restart.
                raise HTTPException(status_code=409, detail="previous paid task requires recovery")
            return {"id": task_id}
        result = await _create_translation(file, data)
        temp_name = None
        try:
            with tempfile.NamedTemporaryFile(mode="w", dir=directory, delete=False) as tmp:
                temp_name = tmp.name
                json.dump(result, tmp)
                tmp.flush()
                os.fsync(tmp.fileno())
            os.replace(temp_name, marker)
        except Exception:
            if temp_name:
                Path(temp_name).unlink(missing_ok=True)
            task = _tasks.pop(result["id"])
            task.worker.cancel()
            shutil.rmtree(task.input_path.parent, ignore_errors=True)
            raise
        return result


def _require_task(task_id: str) -> TaskState:
    task = _tasks.get(task_id)
    if task is None:
        raise HTTPException(status_code=404, detail="task not found")
    return task


@app.get("/v1/translate/{task_id}")
async def get_status(task_id: str) -> dict[str, Any]:
    task = _require_task(task_id)
    return {
        "id": task.task_id,
        "state": task.status,
        "stage": task.stage,
        "progress": task.progress,
        "error": task.error,
        "error_type": task.error_type,
        "total_seconds": task.total_seconds,
        "token_usage": task.token_usage,
        "mono_url": f"/v1/translate/{task.task_id}/mono" if task.mono_path else None,
        "dual_url": f"/v1/translate/{task.task_id}/dual" if task.dual_path else None,
    }


@app.get("/v1/translate/{task_id}/stream")
async def stream_status(task_id: str) -> EventSourceResponse:
    task = _require_task(task_id)

    async def event_generator() -> AsyncGenerator[dict[str, str], None]:
        q = task.subscribe()
        try:
            while True:
                event = await q.get()
                if event is _SENTINEL:
                    break
                yield {"data": json.dumps(event, ensure_ascii=False)}
        finally:
            task.subscribers.discard(q)

    return EventSourceResponse(event_generator())


@app.get("/v1/translate/{task_id}/mono")
async def download_mono(task_id: str) -> FileResponse:
    task = _require_task(task_id)
    if not task.mono_path or not task.mono_path.exists():
        raise HTTPException(status_code=404, detail="mono PDF not available")
    return FileResponse(
        task.mono_path, media_type="application/pdf",
        filename=f"{task_id}-mono.pdf",
    )


@app.get("/v1/translate/{task_id}/dual")
async def download_dual(task_id: str) -> FileResponse:
    task = _require_task(task_id)
    if not task.dual_path or not task.dual_path.exists():
        raise HTTPException(status_code=404, detail="dual PDF not available")
    return FileResponse(
        task.dual_path, media_type="application/pdf",
        filename=f"{task_id}-dual.pdf",
    )


@app.delete("/v1/translate/{task_id}")
async def cancel_task(task_id: str) -> dict[str, str]:
    task = _require_task(task_id)
    if task.worker and not task.worker.done():
        task.worker.cancel()
    _discard_task(task_id)
    return {"id": task_id, "state": "cancelled"}


def _discard_task(task_id: str) -> None:
    task = _tasks.pop(task_id, None)
    if task is None:
        return
    shutil.rmtree(task.output_dir.parent, ignore_errors=True)


async def _cleanup_loop() -> None:
    """Periodically drop finished tasks past their TTL and remove temp files."""
    while True:
        await asyncio.sleep(60)
        now = time.time()
        expired = [
            tid
            for tid, t in list(_tasks.items())
            if t.done and now - t.created_at > TASK_TTL_SECONDS
        ]
        for tid in expired:
            _discard_task(tid)


def run_server(host: str | None = None, port: int | None = None) -> None:
    """Entry point to launch the HTTP API with uvicorn (standalone)."""
    logging.basicConfig(level=logging.INFO)
    asyncio.run(serve(host, port))


async def serve(host: str | None = None, port: int | None = None) -> None:
    """Run the HTTP API inside the current event loop (embeddable)."""
    import uvicorn

    config = uvicorn.Config(
        app,
        host=host or os.environ.get("PDF2ZH_API_HOST", "0.0.0.0"),
        port=port or int(os.environ.get("PDF2ZH_API_PORT", "11008")),
    )
    await uvicorn.Server(config).serve()


if __name__ == "__main__":
    run_server()
