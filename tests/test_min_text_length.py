import pytest
from pdf2zh_next.http_api import TranslateRequest
from pdf2zh_next.http_api import _build_settings
from pydantic import ValidationError


@pytest.mark.parametrize(
    ("lang_in", "expected"),
    [
        ("zh", 2),
        ("zh-TW", 2),
        ("zh_CN", 2),
        ("ZH-Hans", 2),
        ("ja", 2),
        ("ko", 2),
        ("en", 5),
        ("fr", 5),
        ("de", 5),
        ("", 5),
    ],
)
def test_min_text_length_follows_source_language(tmp_path, lang_in, expected):
    # Short CJK table words (居民，项目名称) must be translated; Latin fragments
    # under 5 characters ("No.", "set") stay skipped as BabelDOC intends.
    settings = _build_settings(TranslateRequest(lang_in=lang_in), tmp_path)
    assert settings.translation.min_text_length == expected


def test_explicit_min_text_length_wins(tmp_path):
    req = TranslateRequest(lang_in="zh", min_text_length=5)
    assert _build_settings(req, tmp_path).translation.min_text_length == 5
    req = TranslateRequest(lang_in="en", min_text_length=0)
    assert _build_settings(req, tmp_path).translation.min_text_length == 0


def test_negative_min_text_length_is_rejected():
    with pytest.raises(ValidationError):
        TranslateRequest(min_text_length=-1)
