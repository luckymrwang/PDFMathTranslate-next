import json
import logging
import os
import tempfile
import threading
import time
from pathlib import Path

import requests
from pdf2zh_next.config.model import SettingsModel
from pdf2zh_next.translator.base_rate_limiter import BaseRateLimiter
from pdf2zh_next.translator.base_translator import BaseTranslator
from pdf2zh_next.translator.translator_impl.bing import BingTranslator
from tenacity import before_sleep_log
from tenacity import retry
from tenacity import retry_if_exception_type
from tenacity import stop_after_attempt
from tenacity import wait_exponential

logger = logging.getLogger(__name__)

AZURE_TIMEOUT = (10, 30)
# Bad key / exhausted free quota: Azure keeps refusing until the month rolls over.
BLOCK_AUTH_SECONDS = 30 * 60
# Throttled: only a short breather is needed.
BLOCK_THROTTLE_SECONDS = 60


class AzureUnavailableError(Exception):
    """Azure refuses requests for now (quota, key, throttling); retrying will not help."""

    def __init__(self, message: str, block_seconds: int, status: int = 0):
        super().__init__(message)
        self.block_seconds = block_seconds
        self.status = status


class AzureTransientError(Exception):
    """A server-side error that may succeed on retry."""


# Shared by every translator instance in the process: once the free quota is gone, the
# remaining paragraphs of all tasks go straight to the fallback instead of each probing Azure.
_block_lock = threading.Lock()
_blocked_until = 0.0


def _is_blocked() -> bool:
    with _block_lock:
        return time.monotonic() < _blocked_until


def _block(seconds: int) -> bool:
    """Block Azure for `seconds`; returns True when this call started the block."""
    global _blocked_until
    with _block_lock:
        now = time.monotonic()
        started = now >= _blocked_until
        _blocked_until = max(_blocked_until, now + seconds)
        return started


def _reset_block() -> None:
    global _blocked_until
    with _block_lock:
        _blocked_until = 0.0


# Every task translates in its own subprocess, so the block above is invisible to the
# API process and to other tasks. This small file is how an outage is seen from outside
# (GET /health -> "azure"): written when Azure first stops answering, cleared when it
# answers again. One outage is one record, however many blocks it takes to end.
RECOVERY_CHECK_SECONDS = 30
_last_recovery_check = 0.0


def _state_file() -> Path:
    return Path(
        os.environ.get("AZURE_STATE_FILE")
        or Path(tempfile.gettempdir()) / "pdf2zh-azure-state.json"
    )


def fallback_status() -> dict:
    """{"state": "ok"} or {"state": "fallback", "since", "status", "reason", "last_block"}."""
    try:
        data = json.loads(_state_file().read_text())
    except (OSError, ValueError):
        return {"state": "ok"}
    return (
        data
        if isinstance(data, dict) and data.get("state") == "fallback"
        else {"state": "ok"}
    )


def _write_state(data: dict) -> None:
    path = _state_file()
    try:
        tmp = path.with_name(f"{path.name}.{os.getpid()}.tmp")
        tmp.write_text(json.dumps(data))
        tmp.replace(path)
    except OSError as e:  # reporting must never break a translation
        logger.debug(f"cannot write Azure state: {e}")


def _record_fallback(status: int, reason: str) -> None:
    now = int(time.time())
    current = fallback_status()
    if current["state"] == "fallback":
        current["last_block"] = now  # same outage: keep when it began
        _write_state(current)
        return
    _write_state(
        {
            "state": "fallback",
            "since": now,
            "last_block": now,
            "status": status,
            "reason": reason[:200],
        }
    )


def _record_recovery() -> None:
    """Azure answered: end the outage record (checked at most every RECOVERY_CHECK_SECONDS)."""
    global _last_recovery_check
    now = time.monotonic()
    if now - _last_recovery_check < RECOVERY_CHECK_SECONDS:
        return
    _last_recovery_check = now
    current = fallback_status()
    if current["state"] == "fallback":
        _write_state(
            {
                "state": "ok",
                "recovered": int(time.time()),
                "since": current.get("since", 0),
            }
        )


class AzureTranslator(BaseTranslator):
    name = "azure"
    lang_map = {"zh": "zh-Hans"}

    def __init__(
        self,
        settings: SettingsModel,
        rate_limiter: BaseRateLimiter,
    ):
        super().__init__(settings, rate_limiter)
        engine = settings.translate_engine_settings
        self.endpoint = (engine.azure_endpoint or "").rstrip("/")
        self.region = engine.azure_region
        self.fallback_to_bing = bool(engine.azure_fallback_bing)
        self.headers = {
            "Ocp-Apim-Subscription-Key": engine.azure_api_key,
            "Content-Type": "application/json",
        }
        if self.region:
            self.headers["Ocp-Apim-Subscription-Region"] = self.region
        self.session = requests.Session()
        self._bing = None
        self._bing_lock = threading.Lock()
        self._settings = settings
        self._rate_limiter = rate_limiter
        # Fallback results are stored under the azure cache key too, so a translation
        # the fallback produced is not paid for again on a rerun.

    def _fallback(self):
        with self._bing_lock:
            if self._bing is None:
                self._bing = BingTranslator(self._settings, self._rate_limiter)
            return self._bing

    @retry(
        retry=retry_if_exception_type((requests.RequestException, AzureTransientError)),
        stop=stop_after_attempt(3),
        wait=wait_exponential(multiplier=1, min=1, max=8),
        before_sleep=before_sleep_log(logger, logging.WARNING),
        reraise=True,
    )
    def _azure_translate(self, text):
        response = self.session.post(
            f"{self.endpoint}/translate",
            params={
                "api-version": "3.0",
                "from": self.lang_in,
                "to": self.lang_out,
            },
            headers=self.headers,
            json=[{"Text": text}],
            timeout=AZURE_TIMEOUT,
        )
        status = response.status_code
        if status in (401, 403):
            raise AzureUnavailableError(
                f"Azure rejected the request ({status}): {response.text[:200]}",
                BLOCK_AUTH_SECONDS,
                status,
            )
        if status == 429:
            raise AzureUnavailableError(
                "Azure throttled the request (429)", BLOCK_THROTTLE_SECONDS, 429
            )
        if status >= 500:
            raise AzureTransientError(f"Azure server error {status}")
        response.raise_for_status()
        translated = response.json()[0]["translations"][0]["text"]
        _record_recovery()
        return translated

    def do_translate(self, text, rate_limit_params: dict = None):
        if self.fallback_to_bing and _is_blocked():
            return self._fallback().do_translate(text)
        try:
            return self._azure_translate(text)
        except AzureUnavailableError as e:
            if not self.fallback_to_bing:
                raise
            if _block(e.block_seconds):
                logger.warning(
                    f"{e}; using Bing for the next {e.block_seconds // 60} minutes"
                )
            if (
                e.status != 429
            ):  # throttling clears in a minute; not an outage to report
                _record_fallback(e.status, str(e))
        except Exception as e:
            if not self.fallback_to_bing:
                raise
            logger.warning(f"Azure translation failed ({e!r}); falling back to Bing")
        return self._fallback().do_translate(text)
