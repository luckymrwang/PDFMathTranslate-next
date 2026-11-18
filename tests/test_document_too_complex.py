import pickle
from pathlib import Path

from pdf2zh_next.high_level import BabeldocError
from pdf2zh_next.high_level import DocumentTooComplexError
from pdf2zh_next.high_level import ScannedPDFDetectedError
from pdf2zh_next.http_api import DOCUMENT_TOO_COMPLEX
from pdf2zh_next.http_api import SCANNED_PDF
from pdf2zh_next.http_api import TaskState
from pdf2zh_next.http_api import _serialize_event


def _task():
    return TaskState(task_id="t1", input_path=Path("in.pdf"), output_dir=Path("out"))


def test_error_survives_the_subprocess_pipe():
    error = DocumentTooComplexError("too many objects", original_error="limit hit")
    restored = pickle.loads(pickle.dumps(error))
    assert type(restored) is DocumentTooComplexError
    assert isinstance(restored, BabeldocError)


def test_parser_limit_is_reported_as_a_stable_error_type():
    task = _task()
    event = _serialize_event(
        {
            "type": "error",
            "error": "Babeldoc translation error: limit hit",
            "error_type": "DocumentTooComplexError",
        },
        task,
    )
    assert event["error_type"] == DOCUMENT_TOO_COMPLEX == task.error_type
    assert "过于复杂" in event["error"]


def test_scan_rejection_is_reported_as_a_stable_error_type():
    restored = pickle.loads(pickle.dumps(ScannedPDFDetectedError("x", original_error="y")))
    assert type(restored) is ScannedPDFDetectedError
    task = _task()
    event = _serialize_event(
        {
            "type": "error",
            "error": "Babeldoc translation error: Scanned PDF detected.",
            "error_type": "ScannedPDFDetectedError",
        },
        task,
    )
    assert event["error_type"] == SCANNED_PDF == task.error_type
    assert "扫描件" in event["error"]


def test_other_babeldoc_errors_keep_their_type():
    task = _task()
    event = _serialize_event(
        {"type": "error", "error": "boom", "error_type": "BabeldocError"}, task
    )
    assert event["error_type"] == "BabeldocError"
