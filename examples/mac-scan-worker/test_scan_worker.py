"""Gateway protocol tests for scan_worker.py; the pipeline itself is stubbed.

Run: python -m unittest examples/mac-scan-worker/test_scan_worker.py
"""
import datetime
import hashlib
import json
import os
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
os.environ.setdefault("OCRMYPDF", "/bin/echo")

import httpx  # noqa: E402

import scan_pipeline  # noqa: E402
import scan_worker  # noqa: E402

SOURCE = b"%PDF-1.7 scanned source"


class FakeGateway:
    """Records worker calls and answers like the Go gateway."""

    def __init__(self, source_status=200):
        self.calls, self.uploads, self.fails = [], {}, []
        self.source_status = source_status

    def __call__(self, request: httpx.Request) -> httpx.Response:
        path = request.url.path
        self.calls.append((request.method, path))
        assert request.headers["Authorization"] == "Bearer token"
        if path.endswith("/source"):
            if self.source_status != 200:
                return httpx.Response(self.source_status)
            return httpx.Response(200, content=SOURCE,
                                  headers={"X-File-Sha256": hashlib.sha256(SOURCE).hexdigest()})
        if path.endswith("/progress"):
            return httpx.Response(200, json={"ok": True})
        if path.endswith("/result") or path.endswith("/release"):
            body = request.read()
            for kind in ("dual", "mono"):
                if f'name="{kind}"'.encode() in body:
                    self.uploads[kind] = True
            self.hold = b'name="hold"\r\n\r\n1' in body
            self.report = "质检" in body.decode("utf-8", "ignore")
            return httpx.Response(200, json={"delivery": "held" if self.hold else "archived"})
        if path.endswith("/fail"):
            self.fails.append(json.loads(request.read()))
            return httpx.Response(200, json={"delivery": ""})
        return httpx.Response(404)


def gateway_for(fake):
    return scan_worker.Gateway("https://gw.test", "token", transport=httpx.MockTransport(fake))


JOB = {"id": "T1", "lang_in": "en", "lang_out": "zh", "output": "both"}


class ScanWorkerTest(unittest.TestCase):
    def setUp(self):
        self.root = Path(tempfile.mkdtemp())

    def run_handle(self, fake, process):
        with mock.patch.object(scan_pipeline, "process", process):
            return scan_worker.handle(gateway_for(fake), object(), dict(JOB), self.root)

    def test_success_downloads_verified_source_and_uploads_both_formats(self):
        fake = FakeGateway()

        def process(job, client):
            self.assertEqual(job.source.read_bytes(), SOURCE)
            self.assertEqual((job.lang_in, job.lang_out, job.output), ("en", "zh", "both"))
            out = {}
            for kind in ("dual", "mono"):
                out[kind] = job.workdir / f"{kind}.pdf"
                out[kind].write_bytes(b"%PDF-" + kind.encode())
            return out

        self.assertEqual(self.run_handle(fake, process), 0)
        self.assertEqual(fake.uploads, {"dual": True, "mono": True})
        self.assertFalse(fake.fails)
        self.assertFalse((self.root / "T1").exists(), "work dir kept after delivery")

    def test_quota_exhaustion_pauses_without_using_an_attempt(self):
        fake = FakeGateway()

        def process(job, client):
            raise scan_pipeline.QuotaExhausted("usage limit reached; not asked again before 23:59 UTC+8")

        wait = self.run_handle(fake, process)
        self.assertGreater(wait, 0)
        self.assertEqual(fake.fails[0]["pause"], True)
        self.assertEqual(fake.uploads, {})

    def test_errors_retry_and_missing_source_fails_for_good(self):
        fake = FakeGateway()
        self.run_handle(fake, mock.Mock(side_effect=RuntimeError("model said no")))
        self.assertEqual((fake.fails[0]["retry"], fake.fails[0]["pause"]), (True, False))
        self.assertTrue((self.root / "T1").exists(), "cached pages should survive a retry")

        gone = FakeGateway(source_status=410)
        self.run_handle(gone, mock.Mock())
        self.assertEqual(gone.fails[0]["retry"], False)

    def run_qa_order(self, problems: int):
        fake = FakeGateway()

        def process(job, client):
            self.assertTrue(job.full_qa, "gateway asked for full quality control")
            job.report = {"pages": 80, "terms": 12, "redone": 6,
                          "problems": [{"page": n, "issues": ["残留原文"]} for n in range(1, problems + 1)]}
            path = job.workdir / "dual.pdf"
            path.write_bytes(b"%PDF-dual")
            return {"dual": path}

        with mock.patch.object(scan_pipeline, "process", process):
            scan_worker.handle(gateway_for(fake), object(), dict(JOB, output="dual", full_qa=True), self.root)
        return fake

    def test_large_order_delivered_below_five_problem_pages_else_held(self):
        fake = self.run_qa_order(4)
        self.assertFalse(fake.hold)
        self.assertTrue(fake.report, "quality report must reach the gateway")
        fake = self.run_qa_order(5)
        self.assertTrue(fake.hold)

    def test_release_uploads_replacement(self):
        fake = FakeGateway()
        path = self.root / "fixed.pdf"
        path.write_bytes(b"%PDF-fixed")
        gateway_for(fake).release("T1", {"dual": path})
        self.assertEqual(fake.uploads, {"dual": True})
        self.assertIn(("POST", "/worker/scan/T1/release"), fake.calls)

    def test_night_hours_sleep_between_polls(self):
        at = lambda h: datetime.datetime(2026, 11, 14, h, 30)  # noqa: E731
        self.assertEqual(scan_worker.night_idle(at(2)), 1500)
        self.assertEqual(scan_worker.night_idle(at(6)), 1500)
        self.assertEqual(scan_worker.night_idle(at(7)), 0)
        self.assertEqual(scan_worker.night_idle(at(0)), 0)
        with mock.patch.dict(os.environ, {"NIGHT_HOURS": "23-7"}):
            self.assertEqual(scan_worker.night_idle(at(23)), 1500)
            self.assertEqual(scan_worker.night_idle(at(12)), 0)

    def test_rule_checks_find_leftovers_and_squeezed_text_but_ignore_noise(self):
        R = scan_pipeline.pymupdf.Rect
        lines = [{"bbox": R(0, 0, 100, 10), "text": "within ninety (90) days of 14.09.2020"},
                 {"bbox": R(0, 20, 100, 30), "text": "This sentence was never translated"},
                 {"bbox": R(0, 40, 100, 50), "text": "rau ae he"},
                 {"bbox": R(0, 60, 100, 70), "text": "Visit www.kpezdmc.org.pk today please"}]
        result = {"groups": [{"ids": [0], "tr": "在九十（90）日内"}]}
        issues = scan_pipeline.rule_issues(lines, result, [{"ids": [0], "fs": 4.0, "scale": 0.5}])
        text = " ".join(issues)
        self.assertIn("never translated", text)
        self.assertNotIn("rau", text)
        self.assertNotIn("www", text)
        self.assertIn("过挤", text)
        ok = {"groups": [{"ids": [0], "tr": "九十（90）日内"}, {"ids": [1], "tr": "这句话"}]}
        self.assertEqual(scan_pipeline.rule_issues(lines, ok, [{"ids": [0], "fs": 9, "scale": 1}]), [])

    def test_review_counts_only_serious_issues(self):
        answers = [{"ok": True, "issues": [{"severity": "轻微", "desc": "表格线断点"}]},
                   {"ok": False, "issues": [{"severity": "严重", "desc": "抄送两行文字重叠"},
                                            {"severity": "轻微", "desc": "字号略小"}]}]
        page = mock.Mock()
        with mock.patch.object(scan_pipeline, "_jpeg", return_value="x"), \
                mock.patch.object(scan_pipeline, "_chat", side_effect=answers):
            job = scan_pipeline.Job(source=Path("x"), workdir=self.root)
            self.assertEqual(scan_pipeline.review_page(job, None, page, page), [])
            self.assertEqual(scan_pipeline.review_page(job, None, page, page), ["抄送两行文字重叠"])

    def test_power_cut_order_is_handed_back_free_after_reboot(self):
        fake = FakeGateway()
        gw = gateway_for(fake)
        scan_pipeline.write_atomic(self.root / "current.json", json.dumps({"id": "T9", "started": 1000.0}))
        with mock.patch.object(scan_worker, "boot_time", return_value=2000.0):  # rebooted after it started
            self.assertTrue(scan_worker.recover(gw, self.root))
        self.assertEqual((fake.fails[0]["interrupted"], fake.fails[0]["retry"]), (True, True))
        self.assertFalse((self.root / "current.json").exists())

    def test_crash_without_reboot_uses_an_attempt(self):
        fake = FakeGateway()
        scan_pipeline.write_atomic(self.root / "current.json", json.dumps({"id": "T9", "started": 3000.0}))
        with mock.patch.object(scan_worker, "boot_time", return_value=2000.0):
            scan_worker.recover(gateway_for(fake), self.root)
        self.assertEqual(fake.fails[0]["interrupted"], False)

    def test_recovery_waits_for_the_network(self):
        def offline(request):
            raise httpx.ConnectError("network not up yet")
        scan_pipeline.write_atomic(self.root / "current.json", json.dumps({"id": "T9", "started": 1.0}))
        self.assertFalse(scan_worker.recover(gateway_for(offline), self.root))
        self.assertTrue((self.root / "current.json").exists(), "must retry once the network is up")

    def test_shutdown_hands_back_the_current_order(self):
        fake = FakeGateway()
        scan_worker.remember(self.root, gateway_for(fake), "T7")
        with mock.patch.object(scan_worker.os, "_exit") as exit_:
            scan_worker.on_terminate(15, None)
        exit_.assert_called_once_with(0)
        self.assertEqual((fake.fails[0]["interrupted"], fake.fails[0]["retry"]), (True, True))
        self.assertFalse((self.root / "current.json").exists())
        scan_worker.CURRENT.clear()

    def test_finished_order_clears_its_state(self):
        fake = FakeGateway()

        def process(job, client):
            self.assertTrue((self.root / "current.json").exists(), "state recorded while working")
            path = job.workdir / "dual.pdf"
            path.write_bytes(b"%PDF-x")
            return {"dual": path}

        with mock.patch.object(scan_pipeline, "process", process):
            scan_worker.handle(gateway_for(fake), object(), dict(JOB, output="dual"), self.root)
        self.assertFalse((self.root / "current.json").exists())
        self.assertFalse(scan_worker.CURRENT)

    def test_resume_reuses_ocr_and_skips_damaged_cache(self):
        work = self.root / "job"
        (work / "cache").mkdir(parents=True)
        for name in ("up.pdf", "ocr.pdf"):
            (work / name).write_bytes(b"%PDF-")
        scan_pipeline.write_atomic(work / "straight.json",
                                   json.dumps({"upright": str(work / "up.pdf"), "ocr": str(work / "ocr.pdf")}))
        job = scan_pipeline.Job(source=work / "src.pdf", workdir=work)
        with mock.patch.object(scan_pipeline, "run_ocr", side_effect=AssertionError("OCR ran again")):
            self.assertEqual(scan_pipeline.straighten(job), (work / "up.pdf", work / "ocr.pdf"))
        damaged = work / "cache" / "0001.json"
        damaged.write_text('{"groups": [{"ids": [0], "tr": "半')
        with mock.patch.object(scan_pipeline.pymupdf, "open", side_effect=RuntimeError("re-translating")):
            with self.assertRaisesRegex(RuntimeError, "re-translating"):
                scan_pipeline.translate_page(job, None, work / "up.pdf", work / "ocr.pdf", 0)
        self.assertFalse(damaged.exists(), "a half-written cache file must be dropped, not crash every retry")

    def test_pause_seconds_reads_reset_time(self):
        later = (datetime.datetime.now() + datetime.timedelta(minutes=30)).strftime("%H:%M")
        seconds = scan_worker.pause_seconds(f"not asked again before {later} UTC+8")
        self.assertTrue(25 * 60 < seconds <= 32 * 60, seconds)
        self.assertEqual(scan_worker.pause_seconds("no time given"), 900)

    def test_quota_errors_are_recognized(self):
        class RateLimited(Exception):
            status_code = 429

        self.assertTrue(scan_pipeline._quota_error(RateLimited("x")))
        self.assertTrue(scan_pipeline._quota_error(Exception("The usage limit has been reached")))
        self.assertFalse(scan_pipeline._quota_error(Exception("connection reset")))


if __name__ == "__main__":
    unittest.main()
