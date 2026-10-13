import logging
import re
import threading
import time
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
        self.api_key = getattr(
            settings.translate_engine_settings, "google_api_key", None
        )
        self._tkk = None
        self._tkk_expires_at = 0.0
        self._tkk_lock = threading.Lock()

    @retry(
        retry=retry_if_exception_type((requests.Timeout, requests.ConnectionError)),
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
        tkk = self._get_tkk()
        params = [
            ("client", "webapp"),
            ("sl", self.lang_in or "auto"),
            ("tl", self.lang_out),
            ("hl", "zh-CN"),
            ("dt", "at"),
            ("dt", "bd"),
            ("dt", "ex"),
            ("dt", "ld"),
            ("dt", "md"),
            ("dt", "qca"),
            ("dt", "rw"),
            ("dt", "rm"),
            ("dt", "ss"),
            ("dt", "t"),
            ("otf", "2"),
            ("ssel", "0"),
            ("tsel", "0"),
            ("kc", "1"),
            ("tk", _google_token(text, tkk)),
            ("q", text),
        ]
        response = self.session.post(
            "https://translate.google.com.hk/translate_a/single",
            params=params,
            headers={
                "Referer": "https://translate.google.com.hk/",
                "User-Agent": "Mozilla/5.0",
            },
            timeout=30,
        )
        response.raise_for_status()
        segments = response.json()[0] or []
        translated = "".join(
            segment[0] for segment in segments if segment and segment[0]
        )
        if not translated:
            raise ValueError("Google Translate returned an empty translation")
        return translated

    def _get_tkk(self) -> str:
        with self._tkk_lock:
            if self._tkk and time.monotonic() < self._tkk_expires_at:
                return self._tkk
            response = self.session.get(
                "https://translate.google.com.hk/",
                headers={"User-Agent": "Mozilla/5.0"},
                timeout=15,
            )
            response.raise_for_status()
            match = re.search(r"tkk:'([0-9]+\.[0-9]+)'", response.text)
            if not match:
                raise ValueError("Could not read Google Translate token seed")
            self._tkk = match.group(1)
            self._tkk_expires_at = time.monotonic() + 3600
            return self._tkk


def _google_token(text: str, tkk: str) -> str:
    first, second = (int(part) for part in tkk.split(".", 1))

    def transform(value: int, operations: str) -> int:
        for index in range(0, len(operations) - 2, 3):
            char = operations[index + 2]
            shift = ord(char) - 87 if char >= "a" else int(char)
            if operations[index + 1] == "+":
                shifted = (value & 0xFFFFFFFF) >> shift
            else:
                shifted = (value << shift) & 0xFFFFFFFF
            if operations[index] == "+":
                value = (value + shifted) & 0xFFFFFFFF
            else:
                value ^= shifted
        return value

    value = first
    for byte in text.encode("utf-8"):
        value = transform(value + byte, "+-a^+6")
    value = transform(value, "+-3^+b+-f") ^ second
    value %= 1_000_000
    return f"{value}.{value ^ first}"
