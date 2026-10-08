import asyncio
import tempfile
import unittest
from pathlib import Path
from unittest.mock import AsyncMock, patch

import httpx
import pymupdf

from pdf2zh_next import http_api as api


class PaidPDFAPITest(unittest.IsolatedAsyncioTestCase):
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

    async def test_bad_pdf_rejected(self):
        result = await self.client.post(
            "/v1/pdf/inspect", files={"file": ("document.pdf", b"not a PDF", "application/pdf")}
        )
        self.assertEqual(result.status_code, 422)

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
