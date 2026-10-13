import logging
import unicodedata

import requests
from pdf2zh_next.config.model import SettingsModel
from pdf2zh_next.translator.base_rate_limiter import BaseRateLimiter
from pdf2zh_next.translator.base_translator import BaseTranslator
from tenacity import before_sleep_log
from tenacity import retry
from tenacity import retry_if_exception_type
from tenacity import stop_after_attempt
from tenacity import wait_exponential

logger = logging.getLogger(__name__)


def remove_control_characters(s):
    return "".join(ch for ch in s if unicodedata.category(ch)[0] != "C")


class GoogleTranslator(BaseTranslator):
    name = "google"
    lang_map = {"zh": "zh-CN"}

    def __init__(
        self,
        settings: SettingsModel,
        rate_limiter: BaseRateLimiter,
    ):
        super().__init__(settings, rate_limiter)
        self.session = requests.Session()
        self.api_key = getattr(settings.translate_engine_settings, "google_api_key", None)

    @retry(
        retry=retry_if_exception_type(Exception),
        stop=stop_after_attempt(5),
        wait=wait_exponential(multiplier=1, min=1, max=15),
        before_sleep=before_sleep_log(logger, logging.WARNING),
    )
    def do_translate(self, text, rate_limit_params: dict = None):
        text = text[:5000]  # google translate max length
        if self.api_key:
            result = self._translate_cloud(text)
        else:
            result = self._translate_free(text)
        return remove_control_characters(result)

    def _translate_cloud(self, text: str) -> str:
        # Google Cloud Translation API v2 (Basic).
        response = self.session.post(
            "https://translation.googleapis.com/language/translate/v2",
            params={"key": self.api_key},
            json={
                "q": text,
                "source": self.lang_in,
                "target": self.lang_out,
                "format": "text",
            },
            timeout=30,
        )
        response.raise_for_status()
        return response.json()["data"]["translations"][0]["translatedText"]

    def _translate_free(self, text: str) -> str:
        # Unofficial keyless endpoint; datacenter IPs are often rate-limited (HTTP 429).
        response = self.session.post(
            "https://translate.googleapis.com/translate_a/single",
            params={"client": "gtx", "sl": self.lang_in, "tl": self.lang_out, "dt": "t"},
            data={"q": text},
            timeout=30,
        )
        response.raise_for_status()
        segments = response.json()[0] or []
        return "".join(segment[0] for segment in segments if segment and segment[0])
