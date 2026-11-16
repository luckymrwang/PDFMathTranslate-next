from types import SimpleNamespace

import pytest
from pdf2zh_next.translator.base_translator import BaseTranslator


class FlakyTranslator(BaseTranslator):
    name = "flaky"

    def __init__(self, failures):
        settings = SimpleNamespace(
            translation=SimpleNamespace(lang_in="zh", lang_out="en", ignore_cache=True)
        )
        super().__init__(settings, SimpleNamespace(wait=lambda _params: None))
        self.failures = failures

    def do_translate(self, text, rate_limit_params=None):
        if self.failures.get(text, 0) > 0:
            self.failures[text] -= 1
            raise ConnectionError("reset")
        return text.upper()


def _try(translator, text):
    try:
        return translator.translate(text)
    except ConnectionError:
        return None


def test_failed_paragraphs_are_counted():
    translator = FlakyTranslator({"b": 9, "c": 9})
    assert [_try(translator, t) for t in "abc"] == ["A", None, None]
    assert translator.untranslated_count == 2


def test_a_later_success_clears_the_paragraph():
    translator = FlakyTranslator({"b": 1})
    assert _try(translator, "b") is None
    assert _try(translator, "b") == "B"
    assert translator.untranslated_count == 0


def test_the_same_paragraph_counts_once():
    translator = FlakyTranslator({"b": 9})
    for _ in range(3):
        assert _try(translator, "b") is None
    assert translator.untranslated_count == 1


def test_the_original_error_is_raised():
    translator = FlakyTranslator({"b": 1})
    with pytest.raises(ConnectionError):
        translator.translate("b")
