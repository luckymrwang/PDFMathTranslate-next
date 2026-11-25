from types import SimpleNamespace

import pytest
import requests
from pdf2zh_next.translator.translator_impl import azure as azure_module
from pdf2zh_next.translator.translator_impl.azure import AzureTranslator
from pdf2zh_next.translator.translator_impl.bing import BingTranslator


def _settings(**azure):
    engine = SimpleNamespace(
        azure_endpoint="https://api.cognitive.microsofttranslator.com/",
        azure_api_key="secret",
        azure_region="japaneast",
        azure_fallback_bing=True,
        **azure,
    )
    return SimpleNamespace(
        translation=SimpleNamespace(lang_in="zh", lang_out="en", ignore_cache=True),
        translate_engine_settings=engine,
    )


class FakeResponse:
    def __init__(self, status=200, payload=None, text=""):
        self.status_code = status
        self._payload = payload
        self.text = text

    def json(self):
        return self._payload

    def raise_for_status(self):
        if self.status_code >= 400:
            raise requests.HTTPError(str(self.status_code))


def _bing_ok(text="bing"):
    return FakeResponse(payload=[{"translations": [{"text": text}]}])


def _azure_ok(text="azure"):
    return FakeResponse(payload=[{"translations": [{"text": text}]}])


def _no_sleep(_seconds):
    pass


def _no_sleep(_seconds):
    pass


@pytest.fixture(autouse=True)
def _fast(monkeypatch, tmp_path):
    monkeypatch.setenv("AZURE_STATE_FILE", str(tmp_path / "azure-state.json"))
    monkeypatch.setattr(azure_module, "_last_recovery_check", 0.0)
    azure_module._reset_block()
    monkeypatch.setattr(BingTranslator._translate_chunk.retry, "sleep", _no_sleep)
    monkeypatch.setattr(AzureTranslator._azure_translate.retry, "sleep", _no_sleep)
    yield
    azure_module._reset_block()


def _bing(monkeypatch, responses):
    """A BingTranslator whose page fetch and POSTs are scripted."""
    bing = BingTranslator(_settings(), None)
    fetches = []

    def find_sid():
        fetches.append(1)
        return ("https://www.bing.com/", "ig", "iid", "key", "token")

    calls = []

    def post(url, **_kwargs):
        calls.append(url)
        result = responses.pop(0)
        if isinstance(result, Exception):
            raise result
        return result

    monkeypatch.setattr(bing, "find_sid", find_sid)
    monkeypatch.setattr(bing.session, "post", post)
    return bing, fetches, calls


def test_bing_reuses_page_parameters_across_paragraphs(monkeypatch):
    bing, fetches, calls = _bing(monkeypatch, [_bing_ok(), _bing_ok(), _bing_ok()])
    for _ in range(3):
        assert bing.do_translate("hi") == "bing"
    assert len(fetches) == 1
    assert len(calls) == 3


def test_bing_refetches_parameters_after_a_failure(monkeypatch):
    bad = FakeResponse(payload={"statusCode": 400})
    bing, fetches, _ = _bing(monkeypatch, [bad, _bing_ok()])
    assert bing.do_translate("hi") == "bing"
    assert len(fetches) == 2


def test_bing_retries_up_to_eight_times(monkeypatch):
    reset = requests.ConnectionError("reset")
    bing, _, calls = _bing(monkeypatch, [reset] * 7 + [_bing_ok()])
    assert bing.do_translate("hi") == "bing"
    assert len(calls) == 8


def test_bing_gives_up_with_the_original_error(monkeypatch):
    bing, _, calls = _bing(monkeypatch, [requests.ConnectionError("reset")] * 8)
    with pytest.raises(requests.ConnectionError):
        bing.do_translate("hi")
    assert len(calls) == 8


def _azure(monkeypatch, azure_responses, bing_responses=(), fallback=True):
    settings = _settings()
    settings.translate_engine_settings.azure_fallback_bing = fallback
    translator = AzureTranslator(settings, None)
    azure_calls = []

    def post(url, **kwargs):
        azure_calls.append((url, kwargs))
        result = azure_responses.pop(0)
        if isinstance(result, Exception):
            raise result
        return result

    monkeypatch.setattr(translator.session, "post", post)
    bing, _, bing_calls = _bing(monkeypatch, list(bing_responses))
    monkeypatch.setattr(translator, "_fallback", lambda: bing)
    return translator, azure_calls, bing_calls


def test_azure_request_shape(monkeypatch):
    translator, calls, _ = _azure(monkeypatch, [_azure_ok()])
    assert translator.do_translate("你好") == "azure"
    url, kwargs = calls[0]
    assert url == "https://api.cognitive.microsofttranslator.com/translate"
    assert kwargs["params"] == {"api-version": "3.0", "from": "zh-Hans", "to": "en"}
    assert kwargs["json"] == [{"Text": "你好"}]
    assert kwargs["headers"]["Ocp-Apim-Subscription-Key"] == "secret"
    assert kwargs["headers"]["Ocp-Apim-Subscription-Region"] == "japaneast"


def test_exhausted_quota_switches_to_bing_and_stays_there(monkeypatch):
    quota = FakeResponse(status=403, text="out of call volume quota")
    translator, azure_calls, bing_calls = _azure(
        monkeypatch, [quota], [_bing_ok(), _bing_ok(), _bing_ok()]
    )
    assert [translator.do_translate(t) for t in ("a", "b", "c")] == ["bing"] * 3
    assert len(azure_calls) == 1  # later paragraphs do not probe Azure again
    assert len(bing_calls) == 3


def test_throttling_also_falls_back(monkeypatch):
    translator, azure_calls, _ = _azure(
        monkeypatch, [FakeResponse(status=429)], [_bing_ok()]
    )
    assert translator.do_translate("a") == "bing"
    assert len(azure_calls) == 1


def test_server_errors_retry_then_fall_back(monkeypatch):
    errors = [FakeResponse(status=503)] * 3
    translator, azure_calls, _ = _azure(monkeypatch, errors, [_bing_ok()])
    assert translator.do_translate("a") == "bing"
    assert len(azure_calls) == 3
    # A transient outage must not block Azure for everyone afterwards.
    assert not azure_module._is_blocked()


def test_transient_error_recovers_without_fallback(monkeypatch):
    translator, azure_calls, bing_calls = _azure(
        monkeypatch, [requests.ConnectionError("reset"), _azure_ok()]
    )
    assert translator.do_translate("a") == "azure"
    assert len(azure_calls) == 2
    assert bing_calls == []


def test_without_fallback_errors_surface(monkeypatch):
    translator, _, bing_calls = _azure(
        monkeypatch, [FakeResponse(status=403)], fallback=False
    )
    with pytest.raises(azure_module.AzureUnavailableError):
        translator.do_translate("a")
    assert bing_calls == []


def test_outage_is_recorded_once_with_its_status(monkeypatch):
    assert azure_module.fallback_status() == {"state": "ok"}
    translator, _, _ = _azure(
        monkeypatch,
        [FakeResponse(status=403, text="free quota exceeded")],
        [_bing_ok(), _bing_ok()],
    )
    translator.do_translate("a")
    status = azure_module.fallback_status()
    assert status["state"] == "fallback"
    assert status["status"] == 403
    assert "free quota exceeded" in status["reason"]
    began = status["since"]

    # The block ends and Azure refuses again: still the same outage, same start time.
    azure_module._reset_block()
    translator, _, _ = _azure(monkeypatch, [FakeResponse(status=403)], [_bing_ok()])
    translator.do_translate("b")
    assert azure_module.fallback_status()["since"] == began


def test_throttling_is_not_reported_as_an_outage(monkeypatch):
    translator, _, _ = _azure(monkeypatch, [FakeResponse(status=429)], [_bing_ok()])
    assert translator.do_translate("a") == "bing"
    assert azure_module.fallback_status() == {"state": "ok"}


def test_outage_ends_when_azure_answers_again(monkeypatch):
    translator, _, _ = _azure(monkeypatch, [FakeResponse(status=403)], [_bing_ok()])
    translator.do_translate("a")
    assert azure_module.fallback_status()["state"] == "fallback"

    azure_module._reset_block()  # the 30 minutes are over
    translator, _, _ = _azure(monkeypatch, [_azure_ok()])
    assert translator.do_translate("b") == "azure"
    assert azure_module.fallback_status() == {"state": "ok"}


def test_unwritable_state_file_never_breaks_a_translation(monkeypatch, tmp_path):
    monkeypatch.setenv("AZURE_STATE_FILE", str(tmp_path / "missing-dir" / "state.json"))
    translator, _, _ = _azure(monkeypatch, [FakeResponse(status=401)], [_bing_ok()])
    assert translator.do_translate("a") == "bing"
