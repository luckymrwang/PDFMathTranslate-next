import logging
import re

import requests
from pdf2zh_next.config.model import SettingsModel
from pdf2zh_next.translator.base_rate_limiter import BaseRateLimiter
from pdf2zh_next.translator.base_translator import BaseTranslator
from tenacity import before_sleep_log
from tenacity import retry
from tenacity import retry_if_exception
from tenacity import stop_after_attempt
from tenacity import wait_exponential

logger = logging.getLogger(__name__)


class BingTranslator(BaseTranslator):
    # https://github.com/immersive-translate/old-immersive-translate/blob/6df13da22664bea2f51efe5db64c63aca59c4e79/src/background/translationService.js
    name = "bing"
    lang_map = {"zh": "zh-Hans", "zh-cn": "zh-Hans", "zh-tw": "zh-Hant", "auto": "en"}

    def __init__(
        self,
        settings: SettingsModel,
        rate_limiter: BaseRateLimiter,
    ):
        super().__init__(settings, rate_limiter)
        # Translations cached before long text was split are truncated; a new key skips them.
        self.add_cache_impact_parameters("split_chars", BING_MAX_CHARS)
        self.session = requests.Session()
        self.endpoint = "https://www.bing.com/translator"
        self.headers = {
            "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36 Edg/131.0.0.0",  # noqa: E501
        }

    def find_sid(self):
        response = self.session.get(self.endpoint)
        response.raise_for_status()
        url = response.url[:-10]
        ig = re.findall(r"\"ig\":\"(.*?)\"", response.text)[0]
        iid = re.findall(r"data-iid=\"(.*?)\"", response.text)[-1]
        key, token = re.findall(
            r"params_AbusePreventionHelper\s=\s\[(.*?),\"(.*?)\",", response.text
        )[0]
        return url, ig, iid, key, token

    def do_translate(self, text, rate_limit_params: dict = None):
        # Bing accepts at most 1000 characters; translate long paragraphs in pieces
        # instead of truncating them, which silently dropped the rest of the text.
        chunks = _split_text(text, BING_MAX_CHARS)
        if len(chunks) == 1:
            return self._translate_chunk(text)
        joiner = "" if self.lang_out.lower().startswith(("zh", "ja", "ko")) else " "
        return joiner.join(self._translate_chunk(chunk) for chunk in chunks)

    @retry(
        retry=retry_if_exception(Exception),
        stop=stop_after_attempt(5),
        wait=wait_exponential(multiplier=1, min=1, max=15),
        before_sleep=before_sleep_log(logger, logging.WARNING),
    )
    def _translate_chunk(self, text):
        url, ig, iid, key, token = self.find_sid()
        response = self.session.post(
            f"{url}ttranslatev3?IG={ig}&IID={iid}",
            data={
                "fromLang": self.lang_in,
                "to": self.lang_out,
                "text": text,
                "token": token,
                "key": key,
            },
            headers=self.headers,
        )
        response.raise_for_status()
        return response.json()[0]["translations"][0]["text"]


BING_MAX_CHARS = 1000
# A sentence with its own trailing punctuation and whitespace, so pieces rejoin to the exact text.
_SENTENCE = re.compile(r".+?(?:[.!?;]+(?:\s+|$)|[。！？；]+\s*|\n+|$)", re.S)


def _split_text(text: str, limit: int) -> list[str]:
    """Split text into pieces of at most `limit` characters, preferring sentence ends."""
    if len(text) <= limit:
        return [text]
    chunks, current = [], ""
    for sentence in _SENTENCE.findall(text):
        while len(sentence) > limit:
            # A single sentence longer than the limit: cut at the last space before it.
            if current:
                chunks.append(current)
                current = ""
            cut = sentence.rfind(" ", 0, limit)
            cut = cut + 1 if cut > 0 else limit
            chunks.append(sentence[:cut])
            sentence = sentence[cut:]
        if len(current) + len(sentence) <= limit:
            current += sentence
        else:
            chunks.append(current)
            current = sentence
    if current:
        chunks.append(current)
    return [chunk.strip() for chunk in chunks if chunk.strip()]
