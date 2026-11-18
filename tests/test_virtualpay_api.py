import asyncio
import tempfile
import unittest
from pathlib import Path
from unittest.mock import AsyncMock, patch

import httpx
import pymupdf

from pdf2zh_next import http_api as api


class PaidPDFAPITest(unittest.IsolatedAsyncioTestCase):
    def test_trial_settings_include_only_requested_pages(self):
        request = api.TranslateRequest(
            pages="1-3", only_include_translated_page=True,
            translate_engine_settings={"translate_engine_type": "Bing"},
        )
        settings = api._build_settings(request, Path("/tmp/trial-test"))
        self.assertEqual(settings.pdf.pages, "1-3")
        self.assertTrue(settings.pdf.only_include_translated_page)
        self.assertFalse(api.TranslateRequest().only_include_translated_page)

    async def asyncSetUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.original_dir = api.WORK_DIR
        api.WORK_DIR = Path(self.directory.name)
        api._paid_submit_lock = None
        api._engine_checks.clear()
        api._engine_checked_at.clear()
        api._tasks.clear()
        self.runner = AsyncMock()
        self.patch = patch.object(api, "_run_translation", self.runner)
        self.patch.start()
        self.client = httpx.AsyncClient(
            transport=httpx.ASGITransport(app=api.app),
            base_url="http://test",
        )
        with pymupdf.open() as document:
            document.new_page()
            document.new_page()
            self.pdf = document.tobytes()
            self.encrypted = document.tobytes(
                encryption=pymupdf.PDF_ENCRYPT_AES_256, user_pw="secret", owner_pw="owner"
            )

    async def asyncTearDown(self):
        for task in api._tasks.values():
            if task.worker:
                task.worker.cancel()
        await asyncio.sleep(0)
        api._tasks.clear()
        self.patch.stop()
        await self.client.aclose()
        api.WORK_DIR = self.original_dir
        self.directory.cleanup()

    async def test_actual_page_count_and_encrypted_rejection(self):
        result = await self.client.post(
            "/v1/pdf/inspect", files={"file": ("document.pdf", self.pdf, "application/pdf")}
        )
        self.assertEqual(result.status_code, 200)
        self.assertEqual(result.json()["page_count"], 2)
        result = await self.client.post(
            "/v1/pdf/inspect", files={"file": ("document.pdf", self.encrypted, "application/pdf")}
        )
        self.assertEqual(result.status_code, 422)

    async def test_render_returns_requested_page_as_jpeg(self):
        result = await self.client.post(
            "/v1/pdf/render", files={"file": ("document.pdf", self.pdf, "application/pdf")},
            data={"data": '{"page": 2, "width": 400}'},
        )
        self.assertEqual(result.status_code, 200)
        self.assertEqual(result.headers["content-type"], "image/jpeg")
        self.assertTrue(result.content.startswith(b"\xff\xd8"))
        result = await self.client.post(
            "/v1/pdf/render", files={"file": ("document.pdf", self.pdf, "application/pdf")},
            data={"data": '{"page": 3}'},
        )
        self.assertEqual(result.status_code, 422)
        result = await self.client.post(
            "/v1/pdf/render", files={"file": ("document.pdf", self.pdf, "application/pdf")},
            data={"data": '{"width": 5000}'},
        )
        self.assertEqual(result.status_code, 400)

    async def test_bad_pdf_rejected(self):
        result = await self.client.post(
            "/v1/pdf/inspect", files={"file": ("document.pdf", b"not a PDF", "application/pdf")}
        )
        self.assertEqual(result.status_code, 422)

    @staticmethod
    def _scan_pdf(pages: int, ocr_text: str = "", under_image: bool = False, **text_style) -> bytes:
        pixmap = pymupdf.Pixmap(pymupdf.csRGB, pymupdf.IRect(0, 0, 60, 80), False)
        pixmap.clear_with(240)
        image = pixmap.tobytes("png")
        with pymupdf.open() as document:
            for _ in range(pages):
                page = document.new_page()
                if ocr_text and under_image:
                    page.insert_text((72, 100), ocr_text, **text_style)
                page.insert_image(page.rect, stream=image)
                if ocr_text and not under_image:
                    page.insert_text((72, 100), ocr_text, **text_style)
            return document.tobytes()

    async def _inspect(self, name: str, content: bytes) -> dict:
        result = await self.client.post(
            "/v1/pdf/inspect", files={"file": (name, content, "application/pdf")}
        )
        self.assertEqual(result.status_code, 200, name)
        return result.json()

    async def test_image_only_scan_is_flagged_but_accepted(self):
        self.assertEqual(await self._inspect("scan.pdf", self._scan_pdf(30)),
                         {"page_count": 30, "scanned": True})

    async def test_scans_with_text_nobody_can_see_are_flagged(self):
        # BabelDOC deletes the text, sees no change and rejects all of these as scans.
        code = "{#{QQABSQI1wgowgBaACQ5LEUHiCwqYkJKgLEoMwRAMOEwLyQFABIA=}#}"
        text = "This page already has a searchable OCR text layer."
        for name, content in (
            ("exam.pdf", self._scan_pdf(6, code, fill_opacity=0)),
            ("invisible-ocr.pdf", self._scan_pdf(5, text, render_mode=3)),
            ("ocr-under-image.pdf", self._scan_pdf(5, text, under_image=True)),
        ):
            self.assertTrue((await self._inspect(name, content))["scanned"], name)

    async def test_sparse_visible_text_and_blank_pages_are_not_flagged(self):
        # A line of real text over a picture (posters, brochures, slides) translates
        # fine, although BabelDOC's own scan check rejects it.
        text = "Choose fresh beef and mutton from licensed sellers."
        with pymupdf.open() as document:
            for _ in range(3):
                document.new_page().insert_text((72, 100), text)
            plain = document.tobytes()
        for name, content in (
            ("brochure.pdf", self._scan_pdf(5, text)),
            ("plain.pdf", plain),
            ("blank.pdf", self.pdf),
        ):
            self.assertFalse((await self._inspect(name, content))["scanned"], name)

    async def test_translation_skips_babeldoc_scan_check_only_for_text_pdfs(self):
        text = "Choose fresh beef and mutton from licensed sellers."
        for content, skip in ((self._scan_pdf(3, text), True), (self._scan_pdf(3), False)):
            self.runner.reset_mock()
            result = await self.client.post(
                "/v1/translate", files={"file": ("document.pdf", content, "application/pdf")}
            )
            self.assertEqual(result.status_code, 200)
            await asyncio.sleep(0)
            settings = self.runner.call_args.args[1]
            self.assertIs(settings.pdf.skip_scanned_detection, skip)

    async def test_quote_rejects_unavailable_engine(self):
        with patch("pdf2zh_next.translator.get_translator", side_effect=RuntimeError("private credential error")) as check:
            result = await self.client.post(
                "/v1/pdf/inspect",
                files={"file": ("document.pdf", self.pdf, "application/pdf")},
                data={"data": '{"lang_in":"en","lang_out":"zh"}'},
            )
        self.assertEqual(result.status_code, 503)
        self.assertNotIn("private credential", result.text)
        check.assert_called_once()

    async def test_repeated_paid_upload_creates_one_task(self):
        results = await asyncio.gather(*[
            self.client.post(
                "/v1/translate",
                files={"file": ("document.pdf", self.pdf, "application/pdf")},
                headers={"X-Idempotency-Key": "virtualpay:T12345678"},
            ) for _ in range(4)
        ])
        self.assertTrue(all(result.status_code == 200 for result in results))
        self.assertEqual(len({result.json()["id"] for result in results}), 1)
        self.assertEqual(len(api._tasks), 1)
        await asyncio.sleep(0)
        self.assertEqual(self.runner.call_count, 1)

    async def test_engine_prewarm_reused_and_expiry_rechecked(self):
        with patch("pdf2zh_next.translator.get_translator") as check:
            results = await asyncio.gather(*[
                self.client.post("/v1/engine/check", json={}) for _ in range(3)
            ])
            self.assertTrue(all(r.status_code == 200 for r in results))
            result = await self.client.post("/v1/pdf/inspect",
                files={"file": ("document.pdf", self.pdf, "application/pdf")},
                data={"data": '{"lang_in":"en"}'})
            self.assertEqual(result.status_code, 200)
            self.assertEqual(check.call_count, 1)
            for key in api._engine_checked_at:
                api._engine_checked_at[key] -= 301
            await self.client.post("/v1/engine/check", json={})
            self.assertEqual(check.call_count, 2)

    async def test_engine_check_transforms_deepseek_before_probing(self):
        seen = []
        with patch("pdf2zh_next.translator.get_translator",
                   side_effect=lambda settings: seen.append(type(settings.translate_engine_settings).__name__)):
            result = await self.client.post("/v1/engine/check", json={
                "translate_engine_settings": {
                    "translate_engine_type": "DeepSeek",
                    "deepseek_api_key": "sk-test",
                    "deepseek_model": "deepseek-flash",
                    "deepseek_thinking_mode": "disabled",
                },
            })
        self.assertEqual(result.status_code, 200)
        self.assertEqual(seen, ["OpenAISettings"])

    async def test_failed_prewarm_is_not_cached(self):
        with patch("pdf2zh_next.translator.get_translator", side_effect=RuntimeError("secret")) as check:
            for _ in range(2):
                result = await self.client.post("/v1/engine/check", json={})
                self.assertEqual(result.status_code, 503)
                self.assertNotIn("secret", result.text)
            self.assertEqual(check.call_count, 2)

    async def test_restart_cannot_silently_repeat_paid_job(self):
        first = await self.client.post(
            "/v1/translate", files={"file": ("document.pdf", self.pdf, "application/pdf")},
            headers={"X-Idempotency-Key": "virtualpay:T12345678"},
        )
        self.assertEqual(first.status_code, 200)
        await asyncio.sleep(0)
        api._tasks.clear()  # Simulate worker memory lost, disk markers retained.
        second = await self.client.post(
            "/v1/translate", files={"file": ("document.pdf", self.pdf, "application/pdf")},
            headers={"X-Idempotency-Key": "virtualpay:T12345678"},
        )
        self.assertEqual(second.status_code, 409)
        self.assertEqual(len(api._tasks), 0)


if __name__ == "__main__":
    unittest.main()
