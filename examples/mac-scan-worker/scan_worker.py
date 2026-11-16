"""Mac scan worker: translates scanned PDFs ordered online with GPT-5.6 / GPT-6.

It polls the gateway over HTTPS (no inbound port is needed), downloads the order's
PDF, runs scan_pipeline (Apple OCR + vision model + in-place rewrite) and uploads
the result, which the gateway delivers to the customer at once. Progress is sent
whenever it changes (at most every few seconds) and at least every minute; it renews
the order's lease, and a refunded or re-queued order makes the gateway answer 409,
which stops the work here.

  python scan_worker.py run                   wait for orders forever (launchd runs this)
  python scan_worker.py adopt ORDER_ID        hand a failed/undelivered paid order to the worker
  python scan_worker.py release ORDER_ID [--dual F] [--mono F]
                                              deliver an order held for review (optionally fixed PDFs)
  python scan_worker.py process IN.pdf OUTDIR [--output both] [--lang-in en] [--lang-out zh] [--full-qa]
                                              translate a local file (manual jobs)

Settings come from the environment or ~/.config/pdfmath-scan-worker.env (KEY=VALUE):
  GATEWAY_URL, SCAN_WORKER_TOKEN                      gateway and its shared token
  OPENAI_BASE_URL, OPENAI_API_KEY, VISION_MODEL       model endpoint (image input)
  OCRMYPDF                                            ocrmypdf with the appleocr plugin
  WORK_DIR, TRANSLATE_WORKERS, GLOSSARY_FILE         optional
  LONG_POLL_SECONDS (300), NIGHT_HOURS (1-7), NIGHT_IDLE_SECONDS (1500), HOLD_PROBLEM_PAGES (5)

Waiting uses long polling: the gateway holds each request up to LONG_POLL_SECONDS and
answers as soon as an order is queued. During NIGHT_HOURS the worker additionally
sleeps NIGHT_IDLE_SECONDS between polls (and tells the gateway, so that is not
mistaken for being offline). Large orders (full_qa from the gateway) are delivered
when fewer than HOLD_PROBLEM_PAGES pages still fail quality control, else held.
"""
from __future__ import annotations

import argparse
import datetime
import hashlib
import json
import logging
import os
import re
import signal
import subprocess
import sys
import threading
import time
import traceback
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))  # spawn children re-import us

import httpx  # noqa: E402

import scan_pipeline  # noqa: E402

ENV_FILE = Path.home() / ".config/pdfmath-scan-worker.env"
log = logging.getLogger("scan-worker")


def load_env(path: Path = ENV_FILE):
    if not path.exists():
        return
    for line in path.read_text().splitlines():
        line = line.strip()
        if line and not line.startswith("#") and "=" in line:
            key, value = line.split("=", 1)
            os.environ.setdefault(key.strip(), value.strip().strip('"').strip("'"))


def setting(name: str, default: str | None = None) -> str:
    value = os.environ.get(name, default)
    if value is None or value == "":
        raise SystemExit(f"missing setting {name} (set it in {ENV_FILE})")
    return value


class Gateway:
    def __init__(self, base: str, token: str, transport=None):
        self.http = httpx.Client(base_url=base.rstrip("/"), headers={"Authorization": "Bearer " + token},
                                 timeout=httpx.Timeout(60, read=600), transport=transport)

    def claim(self, wait: int = 0, idle: int = 0) -> dict | None:
        r = self.http.post("/worker/scan/claim", params={"wait": wait, "idle": idle},
                           timeout=httpx.Timeout(60, read=wait + 60))
        if r.status_code == 204:
            return None
        r.raise_for_status()
        return r.json()

    def download(self, order_id: str, dest: Path):
        digest = hashlib.sha256()
        with self.http.stream("GET", f"/worker/scan/{order_id}/source") as r:
            if r.status_code in (409, 410):
                raise PermanentFailure(f"source unavailable (HTTP {r.status_code})")
            r.raise_for_status()
            expected = r.headers.get("X-File-Sha256", "")
            with open(dest, "wb") as f:
                for chunk in r.iter_bytes(1 << 20):
                    f.write(chunk)
                    digest.update(chunk)
        if expected and digest.hexdigest() != expected:
            raise RuntimeError("downloaded PDF does not match the order's hash")

    def progress(self, order_id: str, stage: str, pct: float) -> bool:
        """False when the gateway says this worker must stop (lease lost)."""
        r = self.http.post(f"/worker/scan/{order_id}/progress", json={"stage": stage, "progress": round(pct, 1)})
        if r.status_code == 409:
            return False
        r.raise_for_status()
        return True

    def upload(self, order_id: str, results: dict[str, Path], report: str = "", hold: bool = False,
               action: str = "result"):
        files = {kind: (f"{order_id}-{kind}.pdf", open(path, "rb"), "application/pdf") for kind, path in results.items()}
        data = {"report": report, "hold": "1" if hold else "0"}
        try:
            r = self.http.post(f"/worker/scan/{order_id}/{action}", files=files or None, data=data,
                               timeout=httpx.Timeout(60, write=1800, read=600))
        finally:
            for _, handle, _ in files.values():
                handle.close()
        if r.status_code == 409:
            raise scan_pipeline.Aborted(f"order no longer accepts a {action}: " + r.text[:200])
        r.raise_for_status()

    def fail(self, order_id: str, reason: str, retry: bool, pause: bool = False, interrupted: bool = False,
             timeout: float = 60):
        """retry: count an attempt and re-queue; pause / interrupted: re-queue without counting one."""
        r = self.http.post(f"/worker/scan/{order_id}/fail", timeout=timeout,
                           json={"reason": reason[:300], "retry": retry, "pause": pause, "interrupted": interrupted})
        if r.status_code not in (200, 409):
            r.raise_for_status()

    def status(self, ready: bool, reason: str = "", idle: int = 0):
        """Tell the gateway this Mac is alive but cannot take orders (or is ready again)."""
        self.http.post("/worker/scan/status", json={"ready": ready, "reason": reason[:200], "idle": idle},
                       timeout=30).raise_for_status()

    def release(self, order_id: str, results: dict[str, Path]):
        self.upload(order_id, results, action="release")

    def adopt(self, order_id: str) -> dict:
        r = self.http.post(f"/worker/scan/{order_id}/adopt")
        if r.status_code >= 400:
            raise SystemExit(f"adopt failed: HTTP {r.status_code} {r.text[:300]}")
        return r.json()


class PermanentFailure(Exception):
    """Retrying cannot help (e.g. the source file is gone)."""


def pause_seconds(message: str, default: int = 900) -> int:
    """How long to stop claiming after a usage-limit error ("... before 12:16 UTC+8")."""
    match = re.search(r"before (\d{1,2}):(\d{2})", message)
    if not match:
        return default
    now = datetime.datetime.now()
    reset = now.replace(hour=int(match[1]), minute=int(match[2]), second=0, microsecond=0)
    if reset <= now:
        reset += datetime.timedelta(days=1)
    return min(int((reset - now).total_seconds()) + 60, 12 * 3600)


def model_client():
    from openai import OpenAI
    return OpenAI(base_url=setting("OPENAI_BASE_URL", "https://oc.2026669.xyz:60802/v1"),
                  api_key=setting("OPENAI_API_KEY"), timeout=300, max_retries=1)


# The vision model each engine the customer picked is translated with. The gateway
# translates GPT-5.6 / GPT-6 orders with the same models, so a scanned order is
# translated by the model the customer paid for. VISION_MODEL overrides both.
VISION_MODELS = {"GPT-5.6": "gpt-5.6-luna", "GPT-6": "gpt-6-luna"}


def vision_model(engine: str) -> str:
    override = os.environ.get("VISION_MODEL")
    if override:
        return override
    return VISION_MODELS.get(engine, VISION_MODELS["GPT-5.6"])


def ready(client) -> str:
    """Empty when the Mac can take work, else why not (checked before claiming)."""
    if not Path(os.path.expanduser(setting("OCRMYPDF"))).exists():
        return "ocrmypdf not found"
    try:
        client.models.list()
    except Exception as exc:  # proxy down: do not claim and burn an attempt
        return f"model proxy unavailable: {exc}"
    return ""


# ---------------------------------------------------------------- shutdown safety
# The order being processed is recorded in WORK_DIR/current.json. A normal shutdown
# (SIGTERM from macOS or launchctl) hands it back to the gateway at once; after a power
# cut the file is still there at the next start and the order is handed back then.
# Either way no attempt is used, and the cached OCR and page translations in the
# order's work dir let the next run continue where this one stopped.
CURRENT: dict = {}


def boot_time() -> float:
    out = subprocess.run(["sysctl", "-n", "kern.boottime"], capture_output=True, text=True).stdout
    match = re.search(r"sec = (\d+)", out)
    return float(match[1]) if match else 0.0


def remember(root: Path, gateway: Gateway, order_id: str):
    scan_pipeline.write_atomic(root / "current.json", json.dumps({"id": order_id, "started": time.time()}))
    CURRENT.update(root=root, gateway=gateway, id=order_id)


def forget(root: Path):
    (root / "current.json").unlink(missing_ok=True)
    CURRENT.clear()


def recover(gateway: Gateway, root: Path) -> bool:
    """Hand back an order left unfinished by the previous run; False if not reachable yet."""
    state = root / "current.json"
    if not state.exists():
        return True
    info = json.loads(state.read_text() or "{}")
    rebooted = boot_time() > info.get("started", 0)
    reason = "Mac 关机或断电后重新开机，接着处理" if rebooted else "处理程序异常退出后重启"
    try:
        # A crash without a reboot may be caused by the order itself, so it uses an attempt.
        gateway.fail(info["id"], reason, retry=True, interrupted=rebooted)
    except Exception as exc:
        log.warning("cannot hand back order %s yet: %s", info.get("id"), exc)
        return False
    log.info("handed back interrupted order %s (%s)", info.get("id"), reason)
    state.unlink(missing_ok=True)
    return True


def on_terminate(signum, frame):
    """Shutdown or launchctl stop: hand the current order back, then exit."""
    if CURRENT:
        try:
            CURRENT["gateway"].fail(CURRENT["id"], "Mac 关机或处理程序被停止", retry=True,
                                    interrupted=True, timeout=8)
            (CURRENT["root"] / "current.json").unlink(missing_ok=True)
            log.info("handed back order %s on shutdown", CURRENT["id"])
        except Exception as exc:  # left for recover() at the next start
            log.warning("could not hand back order on shutdown: %s", exc)
    os._exit(0)


class Heartbeat(threading.Thread):
    """Reports progress as it changes and renews the lease; notices a lost lease.

    A new stage or percentage is sent within MIN_GAP seconds, so the customer sees the
    work advance even on a short order; without changes it is resent every INTERVAL
    seconds to keep the lease (20 minutes) alive.
    """

    INTERVAL = 60
    MIN_GAP = 3

    def __init__(self, gateway: Gateway, order_id: str):
        super().__init__(daemon=True)
        self.gateway, self.order_id = gateway, order_id
        self.stage, self.pct = "已开始处理", 0.0
        self.lost = threading.Event()
        self.done = threading.Event()
        self.changed = threading.Event()
        self.sent = (None, None)
        self.sent_at = 0.0

    def update(self, stage: str, pct: float):
        self.stage, self.pct = stage, pct
        self.changed.set()

    def run(self):
        while not self.done.is_set():
            self.changed.wait(self.INTERVAL)
            self.changed.clear()
            # Coalesce a burst of updates into one request.
            gap = self.MIN_GAP - (time.monotonic() - self.sent_at)
            if gap > 0 and self.done.wait(gap):
                return
            if self.done.is_set():
                return
            current = (self.stage, self.pct)
            if current == self.sent and time.monotonic() - self.sent_at < self.INTERVAL:
                continue  # nothing new and the lease is fresh
            try:
                if not self.gateway.progress(self.order_id, *current):
                    self.lost.set()
                    return
                self.sent, self.sent_at = current, time.monotonic()
            except Exception as exc:  # transient; the lease lasts 20 minutes
                log.warning("progress report failed: %s", exc)


def handle(gateway: Gateway, client, job_info: dict, root: Path) -> int:
    """Process one claimed order; returns seconds to pause before claiming again."""
    order_id = job_info["id"]
    workdir = root / order_id
    workdir.mkdir(parents=True, exist_ok=True)
    remember(root, gateway, order_id)
    beat = Heartbeat(gateway, order_id)
    beat.start()
    try:
        source = workdir / "source.pdf"
        gateway.download(order_id, source)
        gateway.progress(order_id, "开始识别", 1)
        job = scan_pipeline.Job(
            source=source, workdir=workdir, lang_in=job_info.get("lang_in", "en"),
            lang_out=job_info.get("lang_out", "zh"), output=job_info.get("output", "dual"),
            model=vision_model(job_info.get("engine", "")),
            workers=int(setting("TRANSLATE_WORKERS", "8")), ocrmypdf=os.path.expanduser(setting("OCRMYPDF")),
            glossary=scan_pipeline.load_glossary(os.environ.get("GLOSSARY_FILE")),
            progress=lambda stage, pct: (beat.update(stage, pct), log.info("%s %s %.0f%%", order_id, stage, pct)),
            should_stop=beat.lost.is_set, full_qa=bool(job_info.get("full_qa")))
        results = scan_pipeline.process(job, client)
        beat.update("上传结果", 99)
        report, hold = "", False
        if job.report:
            report = scan_pipeline.format_report(job.report)
            hold = len(job.report["problems"]) >= int(setting("HOLD_PROBLEM_PAGES", "5"))
        gateway.upload(order_id, results, report=report, hold=hold)
        log.info("order %s %s", order_id, "held for review" if hold else "delivered")
        scan_pipeline.cleanup(workdir)
    except scan_pipeline.QuotaExhausted as exc:
        # Not the order's fault: hand it back without using up an attempt, and wait.
        wait = pause_seconds(str(exc))
        log.error("model quota exhausted; pausing %d min: %s", wait // 60, exc)
        gateway.fail(order_id, f"模型额度用尽，暂停 {wait // 60} 分钟：{exc}", retry=True, pause=True)
        return wait
    except scan_pipeline.Aborted as exc:
        # Lease lost or order refunded; a re-queued order reuses the cached pages.
        log.warning("order %s stopped: %s", order_id, exc)
    except PermanentFailure as exc:
        log.error("order %s cannot be processed: %s", order_id, exc)
        gateway.fail(order_id, str(exc), retry=False)
    except Exception as exc:
        log.error("order %s failed:\n%s", order_id, traceback.format_exc())
        # Keep the work dir: page translations are cached for the retry.
        gateway.fail(order_id, f"{type(exc).__name__}: {exc}", retry=True)
    finally:
        beat.done.set()
        beat.changed.set()  # wake it so it exits now
        forget(root)
    return 0


def prune(root: Path, days: int = 4):
    """Drop work dirs of orders that were never finished here (results expire anyway)."""
    cutoff = time.time() - days * 86400
    for path in root.iterdir():
        if path.is_dir() and path.stat().st_mtime < cutoff:
            scan_pipeline.cleanup(path)


def night_idle(now: datetime.datetime | None = None) -> int:
    """Seconds to sleep between polls at this hour (NIGHT_HOURS, e.g. 1-7 means 01:00-07:00)."""
    now = now or datetime.datetime.now()
    start, _, end = setting("NIGHT_HOURS", "1-7").partition("-")
    start, end = int(start), int(end)
    hour = now.hour
    inside = start <= hour < end if start < end else (hour >= start or hour < end)
    return int(setting("NIGHT_IDLE_SECONDS", "1500")) if inside else 0


def run_forever():
    gateway = Gateway(setting("GATEWAY_URL"), setting("SCAN_WORKER_TOKEN"))
    client = model_client()
    root = Path(os.path.expanduser(setting("WORK_DIR", "~/Library/Application Support/pdfmath-scan-worker")))
    root.mkdir(parents=True, exist_ok=True)
    wait = int(setting("LONG_POLL_SECONDS", "300"))
    signal.signal(signal.SIGTERM, on_terminate)
    signal.signal(signal.SIGINT, on_terminate)
    # Keep the Mac from idle-sleeping for as long as this process lives.
    subprocess.Popen(["/usr/bin/caffeinate", "-i", "-w", str(os.getpid())])
    log.info("scan worker started; gateway %s", setting("GATEWAY_URL"))
    last_problem = ""
    while True:
        idle = night_idle()
        try:
            if not recover(gateway, root):
                time.sleep(30)
                continue
            problem = ready(client)
            if problem:
                if problem != last_problem:
                    log.error("not taking orders: %s", problem)
                last_problem = problem
                try:
                    gateway.status(False, problem, idle=60)
                except Exception as exc:
                    log.warning("status report failed: %s", exc)
                time.sleep(60)
                continue
            last_problem = ""
            prune(root)
            job_info = gateway.claim(wait=wait, idle=idle)
            if job_info:
                log.info("claimed order %s (%s pages, %s, full QA %s)", job_info["id"], job_info.get("pages"),
                         job_info.get("output"), job_info.get("full_qa"))
                pause = handle(gateway, client, job_info, root)
                if pause:
                    time.sleep(pause)
                continue
        except Exception as exc:  # network trouble: retry shortly
            log.warning("poll failed: %s", exc)
            time.sleep(30)
            continue
        if idle:
            time.sleep(idle)


def main():
    load_env()
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    sub = ap.add_subparsers(dest="cmd", required=True)
    sub.add_parser("run")
    adopt = sub.add_parser("adopt")
    adopt.add_argument("order_id")
    release = sub.add_parser("release")
    release.add_argument("order_id")
    release.add_argument("--dual")
    release.add_argument("--mono")
    proc = sub.add_parser("process")
    proc.add_argument("input")
    proc.add_argument("outdir")
    proc.add_argument("--output", default="both", choices=["dual", "mono", "both"])
    proc.add_argument("--lang-in", default="en")
    proc.add_argument("--lang-out", default="zh")
    proc.add_argument("--full-qa", action="store_true", help="glossary + quality control, as for large orders")
    args = ap.parse_args()

    if args.cmd == "run":
        run_forever()
    elif args.cmd == "adopt":
        result = Gateway(setting("GATEWAY_URL"), setting("SCAN_WORKER_TOKEN")).adopt(args.order_id)
        print(json.dumps(result, ensure_ascii=False))
    elif args.cmd == "release":
        files = {k: Path(v) for k, v in (("dual", args.dual), ("mono", args.mono)) if v}
        Gateway(setting("GATEWAY_URL"), setting("SCAN_WORKER_TOKEN")).release(args.order_id, files)
        print(f"order {args.order_id} delivered")
    else:
        out = Path(args.outdir)
        out.mkdir(parents=True, exist_ok=True)
        job = scan_pipeline.Job(
            source=Path(args.input).resolve(), workdir=out / "work", lang_in=args.lang_in, lang_out=args.lang_out,
            output=args.output, model=vision_model(""),
            workers=int(setting("TRANSLATE_WORKERS", "8")), ocrmypdf=os.path.expanduser(setting("OCRMYPDF")),
            glossary=scan_pipeline.load_glossary(os.environ.get("GLOSSARY_FILE")),
            progress=lambda stage, pct: print(f"[{pct:5.1f}%] {stage}", flush=True), full_qa=args.full_qa)
        for kind, path in scan_pipeline.process(job, model_client()).items():
            dest = out / f"{Path(args.input).stem}-{kind}.pdf"
            os.replace(path, dest)
            print(dest)
        if job.report:
            print(scan_pipeline.format_report(job.report))


if __name__ == "__main__":
    main()
