import logging
import threading
import time

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

    def __init__(self, message: str, block_seconds: int):
        super().__init__(message)
        self.block_seconds = block_seconds


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
            )
        if status == 429:
            raise AzureUnavailableError(
                "Azure throttled the request (429)", BLOCK_THROTTLE_SECONDS
            )
        if status >= 500:
            raise AzureTransientError(f"Azure server error {status}")
        response.raise_for_status()
        return response.json()[0]["translations"][0]["text"]

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
        except Exception as e:
            if not self.fallback_to_bing:
                raise
            logger.warning(f"Azure translation failed ({e!r}); falling back to Bing")
        return self._fallback().do_translate(text)
