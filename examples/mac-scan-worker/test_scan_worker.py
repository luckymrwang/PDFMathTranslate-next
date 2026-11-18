"""Gateway protocol tests for scan_worker.py; the pipeline itself is stubbed.

Run: python -m unittest examples/mac-scan-worker/test_scan_worker.py
"""
import datetime
import hashlib
import json
import os
import sys
import tempfile
import threading
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

    def test_progress_is_sent_when_it_changes_not_only_every_minute(self):
        sent = []

        class Recorder:
            def progress(self, order_id, stage, pct):
                sent.append((stage, pct))
                return True

        beat = scan_worker.Heartbeat(Recorder(), "T1")
        beat.MIN_GAP, beat.INTERVAL = 0.05, 30  # a minute-long interval would never fire here
        beat.start()
        try:
            for stage, pct in (("扶正页面", 2), ("翻译 1/2 页", 55), ("生成 PDF", 98)):
                beat.update(stage, pct)
                deadline = datetime.datetime.now() + datetime.timedelta(seconds=2)
                while (stage, pct) not in sent and datetime.datetime.now() < deadline:
                    threading.Event().wait(0.01)
            self.assertEqual(sent[-1], ("生成 PDF", 98))
            self.assertIn(("翻译 1/2 页", 55), sent)
        finally:
            beat.done.set()
            beat.changed.set()
            beat.join(2)
        self.assertFalse(beat.is_alive(), "heartbeat must stop when the order ends")

    def test_lost_lease_stops_the_heartbeat(self):
        class Refused:
            def progress(self, order_id, stage, pct):
                return False

        beat = scan_worker.Heartbeat(Refused(), "T1")
        beat.MIN_GAP = 0
        beat.start()
        beat.update("翻译 1/2 页", 55)
        self.assertTrue(beat.lost.wait(2))
        beat.join(2)
        self.assertFalse(beat.is_alive())

    def test_vision_model_follows_the_order_engine(self):
        with mock.patch.dict(os.environ, {}, clear=False):
            os.environ.pop("VISION_MODEL", None)
            self.assertEqual(scan_worker.vision_model("GPT-5.6"), "gpt-5.6-luna")
            self.assertEqual(scan_worker.vision_model("GPT-6"), "gpt-6-luna")
            self.assertEqual(scan_worker.vision_model(""), "gpt-5.6-luna")
            os.environ["VISION_MODEL"] = "gpt-6.1-sol,gpt-6-luna"
            self.assertEqual(scan_worker.vision_model("GPT-5.6"), "gpt-6.1-sol,gpt-6-luna",
                             "VISION_MODEL overrides the engine's model")

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
            raise scan_pipeline.QuotaExhaustedError("usage limit reached; not asked again before 23:59 UTC+8")

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

    def test_small_order_with_a_light_report_is_never_held(self):
        fake = FakeGateway()

        def process(job, client):
            self.assertFalse(job.full_qa)
            self.assertEqual(job.review_model, "gpt-5.6-luna", "review runs on the cheap model")
            job.report = {"light": True, "pages": 9, "terms": 0, "checked": 8, "redone": 7,
                          "problems": [{"page": n, "issues": ["残留原文"]} for n in range(1, 8)]}
            path = job.workdir / "dual.pdf"
            path.write_bytes(b"%PDF-dual")
            return {"dual": path}

        with mock.patch.object(scan_pipeline, "process", process):
            scan_worker.handle(gateway_for(fake), object(), dict(JOB, output="dual", engine="GPT-6"), self.root)
        self.assertFalse(fake.hold, "7 failing pages still deliver a small order")
        self.assertTrue(fake.report, "the light report still reaches the gateway")
        text = scan_pipeline.format_report({"light": True, "pages": 9, "checked": 8, "redone": 7, "problems": []})
        self.assertIn("自动质检：9 页规则检查，8 页可疑，复核确认 7 页已重做", text)
        self.assertNotIn("术语表", text, "the light check builds no glossary")

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
        rect = scan_pipeline.pymupdf.Rect
        lines = [{"bbox": rect(0, 0, 100, 10), "text": "within ninety (90) days of 14.09.2020"},
                 {"bbox": rect(0, 20, 100, 30), "text": "This sentence was never translated"},
                 {"bbox": rect(0, 40, 100, 50), "text": "rau ae he"},
                 {"bbox": rect(0, 60, 100, 70), "text": "Visit www.kpezdmc.org.pk today please"}]
        result = {"groups": [{"ids": [0], "tr": "在九十（90）日内"}]}
        issues = scan_pipeline.rule_issues(lines, result, [{"ids": [0], "fs": 4.0, "scale": 0.5}])
        text = " ".join(issues)
        self.assertIn("never translated", text)
        self.assertNotIn("rau", text)
        self.assertNotIn("www", text)
        self.assertIn("过挤", text)
        ok = {"groups": [{"ids": [0], "tr": "九十（90）日内"}, {"ids": [1], "tr": "这句话"}]}
        self.assertEqual(scan_pipeline.rule_issues(lines, ok, [{"ids": [0], "fs": 9, "scale": 1}]), [])

    def test_large_pages_are_shrunk_below_the_model_patch_limit(self):
        import base64
        drawing_doc, a4_doc = scan_pipeline.pymupdf.open(), scan_pipeline.pymupdf.open()
        drawing = drawing_doc.new_page(width=3410, height=2424)  # the 1203x855 mm sanyati drawing
        a4 = a4_doc.new_page(width=595, height=842)

        def size(page, dpi=144):
            raw = base64.b64decode(scan_pipeline._jpeg(page, dpi))
            pix = scan_pipeline.pymupdf.Pixmap(raw)
            return pix.width, pix.height

        w, h = size(drawing)
        patches = -(-w // 32) * -(-h // 32)
        self.assertLessEqual(w * h, scan_pipeline.MAX_IMAGE_PIXELS)
        self.assertLess(2 * patches, 30000, "even a review's two images fit the endpoint")
        self.assertAlmostEqual(w / h, 3410 / 2424, places=2, msg="aspect ratio kept")
        self.assertEqual(size(a4), (1190, 1684), "A4 keeps the full 144 dpi")
        self.assertEqual(size(a4, 110), (a4.get_pixmap(dpi=110).width, a4.get_pixmap(dpi=110).height),
                         "uncapped pages render exactly as before")

    def test_rule_checks_find_unspaced_chinese_leftovers(self):
        # Without spaces a whole Chinese sentence is one "word": it used to pass unseen.
        rect = scan_pipeline.pymupdf.Rect
        # autocorrect-disable  # OCR text exactly as read, without CJK spaces
        lines = [{"bbox": rect(0, 0, 100, 10), "text": "本合同自签订之日起生效"},
                 {"bbox": rect(0, 20, 100, 30), "text": "层 数"},
                 {"bbox": rect(0, 40, 100, 50), "text": "刘春华"},
                 {"bbox": rect(0, 60, 100, 70), "text": "0120050269号"}]
        none = {"groups": []}
        issues = scan_pipeline.rule_issues(lines, none, [], "zh")
        self.assertEqual(len(issues), 1)
        self.assertTrue(issues[0].startswith(scan_pipeline.LEFTOVER))
        self.assertIn("本合同自签订之日起生效", issues[0])
        for short in ("层 数", "刘春华", "0120050269号"):
            # autocorrect-enable
            self.assertNotIn(short, issues[0], "labels, names and numbers under 4 characters are not prose")
        self.assertEqual(scan_pipeline.rule_issues(lines, none, [], "en"), [],
                         "Chinese already in an English source is not leftover source text")

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

    def _page_pdf(self, path, lines):
        doc = scan_pipeline.pymupdf.open()
        page = doc.new_page(width=400, height=300)
        for n, text in enumerate(lines):
            page.insert_text((20, 40 + 30 * n), text, fontsize=12)
        doc.save(path)
        return path

    def _model(self, answers, refuse_temperature=False):
        calls = []

        def create(model, messages, **kwargs):  # noqa: ARG001 - mirrors the OpenAI signature
            calls.append({"system": messages[0]["content"], "kwargs": kwargs, "model": model,
                          "images": sum(c["type"] == "image_url" for c in messages[1]["content"])})
            if refuse_temperature and "temperature" in kwargs:
                raise RuntimeError("Unsupported parameter: 'temperature'")
            msg = mock.Mock(content=json.dumps(answers.pop(0), ensure_ascii=False))
            return mock.Mock(choices=[mock.Mock(message=msg)])

        client = mock.Mock()
        client.chat.completions.create = create
        return client, calls

    def _job(self, name, shared=None):
        work = self.root / name
        (work / "cache").mkdir(parents=True)
        return scan_pipeline.Job(source=work / "src.pdf", workdir=work, shared_cache=shared)

    def test_same_page_is_translated_once_across_orders(self):
        pdf = self._page_pdf(self.root / "page.pdf", ["Hello world again", "Second line here"])
        shared = self.root / "_shared"
        answer = {"groups": [{"ids": [0, 1], "tr": "你好"}], "keep": []}
        client, calls = self._model([answer])
        first = scan_pipeline.translate_page(self._job("o1", shared), client, pdf, pdf, 0)
        second = scan_pipeline.translate_page(self._job("o2", shared), client, pdf, pdf, 0)
        self.assertEqual(first, second)
        self.assertEqual(len(calls), 1, "the second order must reuse the first translation")
        self.assertEqual(calls[0]["kwargs"], {"temperature": 0})
        # A redo after a failed quality check asks the model again and replaces the entry.
        client, calls = self._model([{"groups": [{"ids": [0, 1], "tr": "您好"}], "keep": []}])
        redo = scan_pipeline.translate_page(self._job("o3", shared), client, pdf, pdf, 0, feedback=["译文生硬"])
        self.assertEqual(len(calls), 1)
        later = scan_pipeline.translate_page(self._job("o4", shared), client, pdf, pdf, 0)
        self.assertEqual(redo, later)

    def test_logo_lines_kept_by_the_model_are_not_reasked_or_flagged(self):
        pdf = self._page_pdf(self.root / "logo.pdf", ["National University of Singapore", "This course is about statistics"])
        client, calls = self._model([{"groups": [{"ids": [1], "tr": "本课程讲统计"}], "keep": [0]}])
        result = scan_pipeline.translate_page(self._job("o1"), client, pdf, pdf, 0)
        self.assertEqual(len(calls), 1, "a kept logo line must not trigger the orphan pass")
        self.assertEqual(result["keep"], [0])
        lines = scan_pipeline.ocr_lines(scan_pipeline.pymupdf.open(pdf)[0])
        self.assertEqual(scan_pipeline.rule_issues(lines, result, []), [])

    def test_printed_names_are_romanized_across_scripts_in_every_prompt(self):
        # A Chinese name left in an English page is untranslated, not "kept".
        pdf = self._page_pdf(self.root / "name.pdf", ["Property owner", "Liu Chunhua"])
        client, calls = self._model([{"groups": [{"ids": [0, 1], "tr": "x"}], "keep": []}])
        job = self._job("names")
        job.lang_in, job.lang_out = "zh", "en"
        scan_pipeline.translate_page(job, client, pdf, pdf, 0)
        system = calls[0]["system"]
        self.assertIn("简体中文与英语使用同一种文字时保持原样", system)
        self.assertIn("“刘春华”译为“Liu Chunhua”", system)
        self.assertNotIn("logo、签名、人名等", system, "printed names are no longer kept wholesale")
        langs = {"source": "简体中文", "target": "英语"}
        rule = scan_pipeline.NAME_RULE.format(**langs)
        self.assertIn(rule, scan_pipeline.TERMS_PROMPT.format(**langs), "glossary follows the same rule")
        review = scan_pipeline.REVIEW_PROMPT.format(**langs)
        self.assertIn(rule, review, "review checks names by the same rule")
        self.assertNotIn("人名、签名、纯数字", review, "review no longer excuses every name")
        self.assertNotIn("签名、人名", scan_pipeline.ORPHAN_PROMPT)

    def test_review_and_glossary_use_the_cheap_model_translation_keeps_the_paid_one(self):
        pdf = self._page_pdf(self.root / "models.pdf", ["First real sentence here"])
        job = self._job("models")
        job.model, job.review_model = "gpt-6-luna", "gpt-5.6-luna"
        client, calls = self._model([{"terms": []}, {"ok": True, "issues": []},
                                     {"groups": [{"ids": [0], "tr": "第一句"}], "keep": []}])
        scan_pipeline.extract_terms(job, client, pdf, None)
        page = scan_pipeline.pymupdf.open(pdf)[0]
        scan_pipeline.review_page(job, client, page, page)
        scan_pipeline.translate_page(job, client, pdf, pdf, 0, None, ["上一版残留原文"])
        self.assertEqual([c["model"] for c in calls], ["gpt-5.6-luna", "gpt-5.6-luna", "gpt-6-luna"])
        job = self._job("models2")
        job.model = "gpt-6-luna"  # no review model set: everything stays on the order's model
        client, calls = self._model([{"ok": True, "issues": []}])
        scan_pipeline.review_page(job, client, page, page)
        self.assertEqual(calls[0]["model"], "gpt-6-luna")

    def _light_run(self, hits, reviews, error=None):
        job = self._job("light")
        job.output = "dual"
        seen = {"render": [], "check": [], "translate": []}

        def rule_hits(job, ocr, tess, pages):
            return hits

        def render(job, upright, ocr, tess, total, chunk_ids, lo, hi, mono=False, dual=True):
            seen["render"].append((list(chunk_ids), mono, dual))

        def quality(job, client, upright, ocr, tess, pages, lo, hi, confirm=False):
            seen["check"].append((list(pages), confirm))
            if error:
                raise error
            return reviews.pop(0)

        def translate_all(job, client, upright, ocr, tess, pages, lo, hi, feedback=None):
            seen["translate"].append((list(pages), feedback))

        with mock.patch.multiple(scan_pipeline, _rule_hits=rule_hits, _render=render,
                                 _quality_check=quality, _translate_all=translate_all):
            scan_pipeline._light_quality_check(job, object(), "upright", "ocr", None, 3)
        return job, seen

    def test_light_check_costs_nothing_when_the_rules_find_nothing(self):
        job, seen = self._light_run({}, [])
        self.assertEqual(seen, {"render": [], "check": [], "translate": []})
        self.assertEqual(job.report, {}, "no report, so the gateway sends the usual alert")

    def test_light_check_ignores_cramped_text(self):
        # zh->en pages are nearly always shrunk to fit; a redo cannot fix that.
        job, seen = self._light_run({0: ["译文过挤或字号过小（9.8pt）：债权转让协议"],
                                     1: ["译文过挤或字号过小（5.2pt）：x"]}, [])
        self.assertEqual(seen, {"render": [], "check": [], "translate": []})
        self.assertEqual(job.report, {}, "cramped text alone costs nothing and raises no alert")

    def test_light_check_reviews_only_leftover_pages_and_skips_redo_the_review_rejects(self):
        job, seen = self._light_run({1: ["残留原文：本合同自签订之日起生效"]}, [{}])
        self.assertEqual(seen["check"], [([1], True)], "only the flagged page, review must confirm")
        self.assertEqual(seen["render"], [([0], True, False)], "mono for the review, dual untouched")
        self.assertEqual(seen["translate"], [], "unconfirmed leftovers are not translated again")
        self.assertEqual((job.report["checked"], job.report["redone"], job.report["problems"]), (1, 0, []))

    def test_light_check_redoes_confirmed_pages_with_the_findings(self):
        found = {2: ["残留原文：本合同自签订之日起生效"]}
        job, seen = self._light_run({0: ["译文过挤"], 2: ["残留原文：本合同自签订之日起生效"]}, [dict(found), {}])
        self.assertEqual(seen["check"], [([2], True), ([2], True)], "cramped page 0 is not reviewed")
        self.assertEqual(seen["translate"], [([2], found)])
        self.assertEqual(seen["render"][-1], ([0], True, True), "redo renders the delivered pages too")
        self.assertEqual((job.report["checked"], job.report["redone"], job.report["problems"]), (1, 1, []))

    def test_light_check_failure_delivers_the_first_translation(self):
        job, seen = self._light_run({0: ["残留原文：x"]}, [], error=scan_pipeline.QuotaExhaustedError("usage limit"))
        self.assertEqual(seen["translate"], [])
        self.assertIn("QuotaExhaustedError", job.report["review_error"])
        self.assertIn("复核未完成，按首轮译文交付", scan_pipeline.format_report(job.report))

    def test_orphan_pass_sees_the_page(self):
        pdf = self._page_pdf(self.root / "orphan.pdf", ["First real sentence here", "Another real sentence here"])
        client, calls = self._model([{"groups": [{"ids": [0], "tr": "第一句"}], "keep": []},
                                     {"covered": [], "groups": [{"ids": [1], "tr": "第二句"}]}])
        result = scan_pipeline.translate_page(self._job("o1"), client, pdf, pdf, 0)
        self.assertEqual(len(result["groups"]), 2)
        self.assertEqual([c["images"] for c in calls], [1, 1])
        self.assertIn("logo", calls[1]["system"])

    def test_endpoint_refusing_temperature_still_works(self):
        pdf = self._page_pdf(self.root / "temp.pdf", ["Hello world again"])
        job = self._job("o1")
        client, calls = self._model([{"groups": [{"ids": [0], "tr": "你好"}], "keep": []}], refuse_temperature=True)
        scan_pipeline.translate_page(job, client, pdf, pdf, 0)
        self.assertIsNone(job.temperature)
        self.assertEqual([c["kwargs"] for c in calls], [{"temperature": 0}, {}])

    def test_unused_shared_entries_are_pruned(self):
        shared = self.root / scan_worker.SHARED_DIR
        old, fresh = shared / "pages" / "old.json", shared / "pages" / "new.json"
        old.parent.mkdir(parents=True)
        old.write_text("{}")
        fresh.write_text("{}")
        os.utime(old, (0, 0))
        scan_worker.prune(self.root)
        self.assertFalse(old.exists())
        self.assertTrue(fresh.exists() and shared.exists(), "the shared cache itself must survive pruning")

    def _line(self, words, y0=100, y1=112):
        rect = scan_pipeline.pymupdf.Rect
        boxes = [(t, rect(x0, y0, x1, y1)) for t, x0, x1 in words]
        bbox = rect(boxes[0][1].x0, y0, boxes[-1][1].x1, y1)
        return {"bbox": bbox, "text": " ".join(t for t, _ in boxes), "words": boxes}

    def test_icons_beside_a_label_are_not_covered(self):
        line = self._line([("4", 540, 552), ("Lecture", 557, 597)])
        box, src = scan_pipeline.cover_box(line, "讲座", typical=12)
        self.assertEqual((box.x0, src), (557, "Lecture"))
        # a number the translation repeats is part of the text and gets covered
        line = self._line([("2023", 100, 130), ("Annual", 134, 170), ("Report", 174, 210)])
        # autocorrect-disable  # a model translation without a CJK space
        self.assertEqual(scan_pipeline.cover_box(line, "2023年度报告", typical=12)[0].x0, 100)
        # autocorrect-enable

    def test_formula_beside_a_label_is_not_covered(self):
        line = self._line([(">", 192, 214), ("ak", 218, 245), ("-", 249, 266), ("Term", 270, 305),
                           ("(general", 309, 357), ("term)", 361, 398)], y0=166, y1=196)
        box, src = scan_pipeline.cover_box(line, "项（通项）", typical=12)
        self.assertEqual((box.x0, src), (270, "Term (general term)"))

    def test_ordinary_sentences_are_covered_whole(self):
        for words, tr in (([("I", 100, 104), ("agree", 108, 140), ("today", 144, 180)], "我今天同意"),
                          ([("A", 100, 108), ("function", 112, 160), ("maps", 164, 190)], "函数映射")):
            line = self._line(words, y0=100, y1=130)  # tall heading: still no symbol, keep it whole
            self.assertEqual(scan_pipeline.cover_box(line, tr, typical=12)[0], line["bbox"])

    def test_badge_cover_takes_the_badge_colour(self):
        doc = scan_pipeline.pymupdf.open()
        page = doc.new_page(width=200, height=100)
        badge = scan_pipeline.pymupdf.Rect(50, 40, 120, 56)  # blue button on a white page
        page.draw_rect(badge, color=None, fill=(0.2, 0.4, 0.8))
        page.insert_text((60, 52), "Lecture", fontsize=10, color=(1, 1, 1))
        pix = page.get_pixmap(dpi=scan_pipeline.DPI)
        bg, fg = scan_pipeline._colours(pix, scan_pipeline.pymupdf.Rect(58, 43, 100, 54))
        self.assertLess(bg[0], 0.4, f"cover must be the badge blue, got {bg}")
        self.assertGreater(min(fg), 0.8, f"text must stay white, got {fg}")
        plain = doc.new_page(width=200, height=100)
        plain.insert_text((60, 52), "Lecture", fontsize=10)
        bg, fg = scan_pipeline._colours(plain.get_pixmap(dpi=scan_pipeline.DPI),
                                        scan_pipeline.pymupdf.Rect(58, 43, 100, 54))
        self.assertGreater(min(bg), 0.9)
        self.assertLess(max(fg), 0.3)

    @staticmethod
    def _image_pdf(path, width_px, page_w, page_h, text=""):
        pymupdf = scan_pipeline.pymupdf
        height_px = round(width_px * page_h / page_w)
        pix = pymupdf.Pixmap(pymupdf.csRGB, pymupdf.IRect(0, 0, width_px, height_px), False)
        for y in range(0, height_px, 3):  # noisy rows, so lossless compression cannot shrink it
            pix.set_rect(pymupdf.IRect(0, y, width_px, y + 1), ((y * 37) % 256, (y * 11) % 256, (y * 5) % 256))
        with pymupdf.open() as doc:
            page = doc.new_page(width=page_w, height=page_h)
            page.insert_image(page.rect, stream=pix.tobytes("png"))
            if text:
                page.insert_text((20, 40), text, fontsize=12)
            doc.save(path)

    def _merged_image(self, source_px, ocr_px):
        """Merge a 400-ish dpi OCR page for a source scanned at source_px across 4 in."""
        source, part, dest = self.root / "source.pdf", self.root / "dual_0000.pdf", self.root / "result-dual.pdf"
        self._image_pdf(source, source_px, 288, 162)
        self._image_pdf(part, ocr_px, 288, 162, "translation")
        scan_pipeline._merge([part], dest, scan_pipeline.source_dpi(source))
        with scan_pipeline.pymupdf.open(dest) as out:
            xref, width = out[0].get_images(full=True)[0][:3:2]
            self.assertEqual(out.extract_image(xref)["ext"], "jpeg")
            self.assertIn("translation", out[0].get_text())
        self.assertLess(dest.stat().st_size, part.stat().st_size / 2)
        return width

    def test_delivered_pdf_shrinks_the_ocr_images_of_a_low_res_scan(self):
        # A 71 dpi scan comes back from ocrmypdf as a lossless 400 dpi image per page.
        self.assertEqual(self._merged_image(284, 1600), 800)  # 400 -> 200 dpi

    def test_delivered_pdf_keeps_the_resolution_of_a_sharp_scan(self):
        # A 300 dpi scan stays at 300 dpi; only the lossless image becomes a JPEG.
        self.assertEqual(self._merged_image(1200, 1200), 1200)

    def test_source_dpi_reads_the_sharpest_page_within_range(self):
        path = self.root / "s.pdf"
        for px, want in ((284, 150), (800, 200), (2400, 300)):  # 71, 200, 600 dpi
            self._image_pdf(path, px, 288, 162)
            self.assertEqual(scan_pipeline.source_dpi(path), want, px)
        with scan_pipeline.pymupdf.open() as doc:  # vector pages: OCR rasterizes them sharp
            doc.new_page().insert_text((72, 72), "text")
            doc.save(path)
        self.assertEqual(scan_pipeline.source_dpi(path), 300)

    @staticmethod
    def _long_page(path, blocks=10):
        """A 400 x 6000 pt page: text blocks 600 pt apart with blank gaps between."""
        pymupdf = scan_pipeline.pymupdf
        with pymupdf.open() as doc:
            page = doc.new_page(width=400, height=600 * blocks)
            for b in range(blocks):
                for k in range(8):  # each block is 8 lines, y = 600b + 100 .. 380
                    page.insert_text((30, 600 * b + 100 + 40 * k), f"block {b} line {k} " * 3, fontsize=14)
            doc.save(path)

    def test_ordinary_pages_are_not_tiled(self):
        pymupdf = scan_pipeline.pymupdf
        with pymupdf.open() as doc:
            for w, h in ((595, 842), (842, 595), (400, 950)):
                self.assertEqual(scan_pipeline.tile_cuts(doc.new_page(width=w, height=h)), [], (w, h))

    def test_long_page_is_cut_in_the_gaps_between_text(self):
        path = self.root / "long.pdf"
        self._long_page(path)
        with scan_pipeline.pymupdf.open(path) as doc:
            cuts = scan_pipeline.tile_cuts(doc[0])
        self.assertGreater(len(cuts), 5)
        for cut in cuts:  # text sits at 600b + 85 .. 385; the rest is blank
            self.assertFalse(85 <= cut % 600 <= 385, cut)
            self.assertLessEqual(cut - (cuts[cuts.index(cut) - 1] if cuts.index(cut) else 0),
                                 1.25 * scan_pipeline.TILE_RATIO * 400 + 1)

    def test_tiles_are_stitched_back_into_the_original_page(self):
        pymupdf = scan_pipeline.pymupdf
        source = self.root / "long.pdf"
        self._long_page(source)
        job = scan_pipeline.Job(source=source, workdir=self.root)
        layout = scan_pipeline.split_long_pages(job)
        with pymupdf.open(self.root / "tiles.pdf") as tiles:
            self.assertEqual(tiles.page_count, len(layout))
            self.assertTrue(all(abs(t.rect.height - (y1 - y0)) < 0.5 for t, (_, y0, y1) in zip(tiles, layout, strict=True)))
            # A dual result: each tile page twice as wide, as _render_chunk builds it.
            dual = pymupdf.open()
            for t in tiles:
                page = dual.new_page(width=t.rect.width * 2, height=t.rect.height)
                page.show_pdf_page(pymupdf.Rect(0, 0, t.rect.width, t.rect.height), tiles, t.number)
                page.show_pdf_page(pymupdf.Rect(t.rect.width, 0, t.rect.width * 2, t.rect.height), tiles, t.number)
            dest = self.root / "result-dual.pdf"
            dual.save(dest)
        scan_pipeline.stitch_tiles(dest, layout)
        with pymupdf.open(dest) as out:
            self.assertEqual(out.page_count, 1)
            self.assertEqual((round(out[0].rect.width), round(out[0].rect.height)), (800, 6000))
            hit = out[0].search_for("block 7 line 3")
            self.assertTrue(hit)
            self.assertAlmostEqual(hit[0].y1, 600 * 7 + 100 + 40 * 3, delta=6)  # back in place

    def test_pause_seconds_reads_reset_time(self):
        later = (datetime.datetime.now() + datetime.timedelta(minutes=30)).strftime("%H:%M")
        seconds = scan_worker.pause_seconds(f"not asked again before {later} UTC+8")
        self.assertTrue(25 * 60 < seconds <= 32 * 60, seconds)
        self.assertEqual(scan_worker.pause_seconds("no time given"), 900)

    def test_quota_errors_are_recognized(self):
        class RateLimitedError(Exception):
            status_code = 429

        self.assertTrue(scan_pipeline._quota_error(RateLimitedError("x")))
        self.assertTrue(scan_pipeline._quota_error(Exception("The usage limit has been reached")))
        self.assertFalse(scan_pipeline._quota_error(Exception("connection reset")))


if __name__ == "__main__":
    unittest.main()
