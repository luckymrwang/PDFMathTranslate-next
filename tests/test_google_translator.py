from types import SimpleNamespace

from pdf2zh_next.translator.translator_impl.google import GoogleTranslator
from pdf2zh_next.translator.translator_impl.google import _google_token


def test_google_token_matches_webapp_algorithm_for_unicode():
    tkk = "434674.96463358"
    assert _google_token("Hello world", tkk) == "771228.877934"
    assert _google_token("你好，世界", tkk) == "945824.577362"
    assert _google_token("𠜎 test", tkk) == "282620.192014"


def test_free_google_request_uses_tkk_and_joins_segments():
    calls = []

    class Response:
        def raise_for_status(self):
            pass

        def json(self):
            return [[
                ["你好", "Hello", None],
                ["世界", "world", None],
            ]]

    class Session:
        def post(self, url, **kwargs):
            calls.append((url, kwargs))
            return Response()

    translator = GoogleTranslator.__new__(GoogleTranslator)
    translator.session = Session()
    translator.lang_in = "en"
    translator.lang_out = "zh-CN"
    translator._get_tkk = lambda: "434674.96463358"

    assert translator._translate_free("Hello world") == "你好世界"
    url, request = calls[0]
    assert url == "https://translate.google.com.hk/translate_a/single"
    params = request["params"]
    assert ("client", "webapp") in params
    assert ("sl", "en") in params
    assert ("tl", "zh-CN") in params
    assert ("tk", "771228.877934") in params
    assert ("q", "Hello world") in params