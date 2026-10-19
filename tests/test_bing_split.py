import re

from pdf2zh_next.translator.translator_impl.bing import BING_MAX_CHARS
from pdf2zh_next.translator.translator_impl.bing import _split_text


def _no_space(text):
    return re.sub(r"\s+", "", text)


def test_short_text_is_untouched():
    assert _split_text("Hello world.", BING_MAX_CHARS) == ["Hello world."]


def test_long_paragraph_is_split_at_sentences_without_losing_text():
    paragraph = "This sentence is about forty characters. " * 40
    chunks = _split_text(paragraph, BING_MAX_CHARS)
    assert len(chunks) > 1
    assert all(0 < len(chunk) <= BING_MAX_CHARS for chunk in chunks)
    assert all(chunk.endswith(".") for chunk in chunks)
    assert _no_space("".join(chunks)) == _no_space(paragraph)


def test_overlong_sentence_and_unbroken_text_are_hard_split():
    for text in (" ".join(["word"] * 500), "x" * 2500):
        chunks = _split_text(text, BING_MAX_CHARS)
        assert all(0 < len(chunk) <= BING_MAX_CHARS for chunk in chunks)
        assert _no_space("".join(chunks)) == _no_space(text)


def test_chinese_text_is_not_altered():
    text = "这是一个测试句子。" * 233
    chunks = _split_text(text, BING_MAX_CHARS)
    assert all(len(chunk) <= BING_MAX_CHARS for chunk in chunks)
    assert "".join(chunks) == text


def test_urls_and_formula_placeholders_stay_whole():
    text = "a " * 480 + "https://github.com/microsoft/MMA-Sim. " + "See {v1} and {v2} here. " * 30
    chunks = _split_text(text, BING_MAX_CHARS)
    assert any("https://github.com/microsoft/MMA-Sim." in chunk for chunk in chunks)
    assert all(chunk.count("{") == chunk.count("}") for chunk in chunks)
