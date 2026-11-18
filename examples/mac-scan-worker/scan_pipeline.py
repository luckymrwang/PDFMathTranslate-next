"""Scanned-PDF translation that keeps the original page look (runs on macOS).

Pipeline:
  1. OCR         ocrmypdf --ocr-engine appleocr --force-ocr --rotate-pages turns every
                 page upright (Apple orientation check) and gives line boxes and text
  2. backup OCR  Tesseract adds lines that Apple OCR skipped (merged where no Apple line is)
  3. translate   a vision model sees the page image + numbered OCR lines, groups
                 wrapped lines and returns one translation per group (cached per page)
  4. render      each group's lines are covered with the sampled background colour
                 and the translation is written in the sampled text colour; rendering
                 runs in short-lived processes (MuPDF keeps CJK font data alive)
Outputs: dual (original | translation side by side) and/or mono (translation only).
"""
from __future__ import annotations

import base64
import collections
import hashlib
import json
import multiprocessing as mp
import os
import re
import shutil
import statistics
import subprocess
import sys
import time
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass, field
from pathlib import Path
from typing import Callable

import pymupdf

FONT_DIR = Path(os.environ.get("FONT_DIR", Path.home() / ".cache/babeldoc/fonts"))
DPI = 200
SCALE = DPI / 72
BAKE_DPI = 250
CHUNK = 8

LANG_NAMES = {"zh": "简体中文", "en": "英语", "ja": "日语", "ko": "韩语", "fr": "法语", "de": "德语", "es": "西班牙语"}
OCR_LANGS = {"zh": "chi_sim", "en": "eng", "ja": "jpn", "ko": "kor", "fr": "fra", "de": "deu", "es": "spa"}
FONTS = {"ja": "SourceHanSansJP", "ko": "SourceHanSansKR"}  # everything else: SourceHanSansCN

# One rule for names, shared by translation, glossary and review: a printed name
# left in a script the reader cannot read is untranslated, not "kept".
NAME_RULE = ("人名：手写签名保持原样；印刷的人名要处理——{source}与{target}使用同一种文字时保持原样，"
             "不同时按惯例音译：中文人名用汉语拼音，姓在前、名连写、不标声调（如“刘春华”译为“Liu Chunhua”），"
             "日文人名用罗马字；译成中文时，拉丁字母人名保持原样。不要按字义意译人名。")

SYSTEM = """你是专业的文档译者。用户会发来一页扫描文档的图片，以及从图上 OCR 出来的文字行列表（JSON，每行有 id、text 和 box=[x0,y0,x1,y1]，单位 pt，页面宽高为 {width}x{height}）。
请结合图片，把文字从{source}翻译成{target}，只输出一个 JSON 对象：
{{"groups": [{{"ids": [<id>, ...], "tr": "<译文>"}}, ...], "keep": [<id>, ...]}}
其中 keep 列出按下面规则特意保持原样、不翻译的文字行（logo、印章、手写签名，以及按人名规则保持原样的人名），没有就给空数组。

分组规则：
- 同一句话/同一段落因换行被拆成的多行（上下相邻、对齐、属于同一段）合并为一组，ids 按阅读顺序列出。
- 单独成行的纯编号（如 2.6.1、(a)、(ii)、1.）不要翻译、不要放进任何组，它们保留在原图上。
- 带文字的标题要翻译（如 “Section 14.1” 译为 “第14.1条”，“ARTICLE 14” 译为 “第14条”）。
- 表格的不同行、不同单元格、不同卡片、不同项目符号条目一律分开，不要合并。
- 每个 id 只能出现在一个组里；不需要翻译的行（纯数字、金额、百分比、日期数字、公式、代码、页码、签名、logo）不要输出。
- 公式保持原样：整行都是公式或数学式（含 =、×、Σ、Π、上下标等）的不要输出，也不要改写成 LaTeX。如果 OCR 把图上的公式和它旁边的标注读成了同一行（如 “Σ a_k ← Term (general term)”，公式和标注之间常有箭头），译文只写标注（“通项”），不要带公式和箭头。
- 句子里出现的变量和简单表达式（如 “the sum from k = 1 to n of a_k”）是句子的一部分，照常写进译文（“从 k = 1 到 n 的 a_k 之和”），不要省略。
- logo 一律保持原样：标志、徽章、印章里的文字，以及与标志排在一起构成标识的机构名、院系名、品牌名、口号（如校徽下方的校名、标志右侧的“学院名 + 校名”组合）都算 logo，不要输出。正文、标题、表格里提到的机构名照常翻译。

翻译要求：
- OCR 可能有错字，以图片为准纠正后再翻译。
- 数字、金额、日期、期限、条款编号、交叉引用、公式、变量名、代码、单位必须与图片完全一致。
- """ + NAME_RULE + """
- 使用该领域规范术语，译文简洁，长度尽量不超过原文所占空间。不要用括号附注原文。"""


class Aborted(Exception):
    """Raised when the caller asks the pipeline to stop (e.g. the order was refunded)."""


class QuotaExhausted(Exception):
    """Every configured model hit its usage limit; the order should wait, not fail."""


def _quota_error(exc: Exception) -> bool:
    text = str(exc).lower()
    return getattr(exc, "status_code", None) == 429 or "usage limit" in text or "quota" in text


@dataclass
class Job:
    source: Path
    workdir: Path
    lang_in: str = "en"
    lang_out: str = "zh"
    output: str = "dual"  # dual | mono | both
    model: str = "gpt-5.6-luna"  # comma-separated: fallbacks used when one runs out of quota
    # Quality review and the glossary only judge or list, so a cheaper model does;
    # translations and redos stay on `model`, the one the customer paid for. Empty: `model`.
    review_model: str = ""
    workers: int = 8
    render_procs: int = 3
    ocrmypdf: str = "ocrmypdf"
    glossary: str = ""
    progress: Callable[[str, float], None] = lambda stage, pct: None
    should_stop: Callable[[], bool] = lambda: False
    full_qa: bool = False  # glossary + vision review of every page + one redo (large orders)
    # Every other order: free rule checks on every page; only pages they flag get a
    # vision review, and only pages the review confirms are translated again.
    light_qa: bool = True
    # Page translations and glossaries shared across orders, keyed by the OCR content,
    # model, languages and prompts: the same file ordered again gets the same result.
    shared_cache: Path | None = None
    # 0 keeps answers as repeatable as the model allows; dropped if the endpoint refuses it.
    temperature: float | None = 0
    report: dict = field(default_factory=dict)
    results: dict = field(default_factory=dict)
    exhausted: set = field(default_factory=set)


def write_atomic(path: Path, text: str):
    """Write via a temp file + rename, so a power cut never leaves half a file behind."""
    tmp = path.with_name(path.name + ".tmp")
    tmp.write_text(text)
    os.replace(tmp, path)


def _shared_path(job: Job, kind: str, *parts) -> Path | None:
    """Cross-order cache file for this content, or None when sharing is off."""
    if job.shared_cache is None:
        return None
    digest = hashlib.sha256(json.dumps(
        [kind, job.model, job.lang_in, job.lang_out, *parts], ensure_ascii=False, sort_keys=True,
        default=str).encode()).hexdigest()
    return job.shared_cache / kind / f"{digest}.json"


def _read_shared(path: Path | None):
    if path is None or not path.exists():
        return None
    try:
        data = json.loads(path.read_text())
    except ValueError:
        path.unlink(missing_ok=True)
        return None
    os.utime(path)  # recently used entries survive pruning
    return data


def _write_shared(path: Path | None, data):
    if path is not None:
        path.parent.mkdir(parents=True, exist_ok=True)
        write_atomic(path, json.dumps(data, ensure_ascii=False))


def prune_shared(root: Path, days: int = 30):
    """Drop shared cache entries nobody used for `days` days."""
    cutoff = time.time() - days * 86400
    for path in root.glob("*/*.json"):
        if path.stat().st_mtime < cutoff:
            path.unlink(missing_ok=True)


def _check(job: Job):
    if job.should_stop():
        raise Aborted("stopped")


# ---------------------------------------------------------------- straighten / OCR
def run_ocr(job: Job, src: Path, dest: Path, rotate: bool = False):
    part = dest.with_name(dest.stem + ".part.pdf")  # renamed only when complete
    lang = OCR_LANGS.get(job.lang_in, "eng")
    cmd = [job.ocrmypdf, "--ocr-engine", "appleocr", "--force-ocr", "--oversample", "300",
           "--output-type", "pdf", "--optimize", "0", "-j", "8", "-l", lang, str(src), str(part)]
    if rotate:
        cmd.insert(1, "--rotate-pages")
    proc = subprocess.run(cmd, capture_output=True, text=True)
    if proc.returncode != 0 and lang != "eng":
        cmd[cmd.index("-l") + 1] = "eng"
        proc = subprocess.run(cmd, capture_output=True, text=True)
    if proc.returncode != 0:
        raise RuntimeError("OCR failed: " + (proc.stderr or proc.stdout)[-400:])
    os.replace(part, dest)


def _image_page(out: pymupdf.Document, page: pymupdf.Page):
    """Append page as a flat image with its /Rotate baked in."""
    pix = page.get_pixmap(dpi=BAKE_DPI, alpha=False)
    target = out.new_page(width=page.rect.width, height=page.rect.height)
    target.insert_image(target.rect, stream=pix.tobytes("jpeg", jpg_quality=88))


def straighten(job: Job) -> tuple[Path, Path]:
    """Return (page images, OCR text) PDFs with every page the right way up.

    With --force-ocr --rotate-pages, ocrmypdf rasterizes each page, uses Apple OCR's
    orientation check to turn sideways or upside-down content upright, and lays the
    text over the same image. Pages with a /Rotate flag are baked into an image first. So the OCR output
    is both the page images and the text, in one coordinate system. Any page left
    with a /Rotate flag is baked and re-OCR'd as a fallback.
    """
    done = job.workdir / "straight.json"
    if done.exists():  # resumed after a shutdown: OCR is already done
        paths = json.loads(done.read_text())
        if all(Path(v).exists() for v in paths.values()):
            return Path(paths["upright"]), Path(paths["ocr"])
    up, ocr = _straighten(job)
    write_atomic(done, json.dumps({"upright": str(up), "ocr": str(ocr)}))
    return up, ocr


def _straighten(job: Job) -> tuple[Path, Path]:
    # ocrmypdf squashes pages that carry a /Rotate flag, so bake those first.
    src = pymupdf.open(job.source)
    source = job.source
    if any(page.rotation % 360 for page in src):
        flat = pymupdf.open()
        for page in src:
            if page.rotation % 360:
                _image_page(flat, page)
            else:
                flat.insert_pdf(src, from_page=page.number, to_page=page.number)
        source = job.workdir / "flat.pdf"
        flat.save(source, garbage=3, deflate=True)
    ocr_path = job.workdir / "ocr.pdf"
    job.progress("识别文字并扶正页面（OCR）", 5)
    run_ocr(job, source, ocr_path, rotate=True)
    _check(job)
    ocr = pymupdf.open(ocr_path)
    turned = [i for i, page in enumerate(ocr) if page.rotation % 360]
    if not turned:
        return ocr_path, ocr_path
    baked = pymupdf.open()
    for i in turned:
        _image_page(baked, ocr[i])
    baked_path, baked_ocr = job.workdir / "turned.pdf", job.workdir / "turned-ocr.pdf"
    baked.save(baked_path, garbage=3, deflate=True)
    run_ocr(job, baked_path, baked_ocr)
    fixed = pymupdf.open(baked_ocr)
    for n, i in enumerate(turned):
        ocr.delete_page(i)
        ocr.insert_pdf(fixed, from_page=n, to_page=n, start_at=i)
    upright = job.workdir / "ocr-upright.pdf"
    ocr.save(upright, garbage=3, deflate=True)
    return upright, upright


# ---------------------------------------------------------------- translation
def _page_lines(page: pymupdf.Page) -> list[dict]:
    lines = []
    for block in page.get_text("dict")["blocks"]:
        for line in block.get("lines", []):
            text = re.sub(r"\s+", " ", " ".join(s["text"] for s in line["spans"])).strip()
            if text and re.search(r"[^\W\d_]{2,}", text):
                # OCR writes one span per word: keep their boxes to trim the cover later.
                words = [(s["text"].strip(), pymupdf.Rect(s["bbox"])) for s in line["spans"] if s["text"].strip()]
                lines.append({"bbox": pymupdf.Rect(line["bbox"]), "text": text, "words": words})
    return lines


def _letters(token: str) -> int:
    return len(re.findall(r"[^\W\d_]", token))


def cover_box(line: dict, translation: str, typical: float) -> tuple[pymupdf.Rect, str]:
    """The part of an OCR line the translation replaces, and its source text.

    OCR often reads an icon or a formula into the same line as the label beside it
    ("4 Lecture", "> ak - Term (general term)"). Covering the whole line erases the
    icon or formula, so leading/trailing tokens are left on the page when they are
    symbols, digits or icon glyphs the translation does not repeat. On a line much
    taller than the page's text (a big formula sign next to a label) short letter
    tokens such as variable names are left too, if a symbol sits among them.
    """
    words = line.get("words") or []
    if len(words) < 2:
        return line["bbox"], line["text"]
    carried = re.sub(r"[\s_]", "", translation)
    tall = line["bbox"].height >= 1.6 * typical

    def symbol(token):  # no letters, and not carried into the translation
        return _letters(token) == 0 and re.sub(r"\s", "", token) not in carried

    def droppable(token):
        return symbol(token) or (tall and 0 < _letters(token) < 3)

    start, end = 0, len(words)
    while start < end and droppable(words[start][0]):
        start += 1
    while end > start and droppable(words[end - 1][0]):
        end -= 1
    dropped = words[:start] + words[end:]
    kept = words[start:end]
    if not dropped or not any(re.search(r"[^\W\d_]{3,}", w) for w, _ in kept):
        return line["bbox"], line["text"]
    if any(_letters(w) for w, _ in dropped) and not any(symbol(w) for w, _ in dropped):
        return line["bbox"], line["text"]  # only short words: an ordinary sentence ("I agree ...")
    box = pymupdf.Rect(kept[0][1].x0, line["bbox"].y0, kept[-1][1].x1, line["bbox"].y1)
    return box, " ".join(w for w, _ in kept)


def _overlaps(a: pymupdf.Rect, b: pymupdf.Rect) -> bool:
    v = min(a.y1, b.y1) - max(a.y0, b.y0)
    h = min(a.x1, b.x1) - max(a.x0, b.x0)
    return v > 0.4 * min(a.height, b.height) and h > 0.3 * min(a.width, b.width)


def ocr_lines(page: pymupdf.Page, backup: pymupdf.Page | None = None) -> list[dict]:
    """Apple OCR lines, plus Tesseract lines where Apple OCR dropped a whole line.

    Apple OCR is more accurate but occasionally skips a line of a paragraph, which
    then stays untranslated and uncovered. Tesseract lines that overlap no Apple line
    fill those holes; the vision model corrects their text from the image.
    """
    lines = _page_lines(page)
    if backup is not None:
        found = [l["bbox"] for l in lines]
        extra = [l for l in _page_lines(backup)
                 if len(re.findall(r"[^\W\d_]{2,}", l["text"])) >= 2
                 and not any(_overlaps(l["bbox"], b) for b in found)]
        if extra:
            lines = sorted(lines + extra, key=lambda l: (round(l["bbox"].y0 / 4), l["bbox"].x0))
    return lines


def run_backup_ocr(job: Job, upright: Path) -> Path | None:
    """Tesseract pass over the upright pages (same geometry); None if unavailable."""
    dest = job.workdir / "tess.pdf"
    if dest.exists():
        return dest
    lang = OCR_LANGS.get(job.lang_in, "eng")
    part = job.workdir / "tess.part.pdf"
    for attempt in (lang, "eng"):
        proc = subprocess.run([job.ocrmypdf, "--ocr-engine", "tesseract", "--force-ocr", "--output-type", "pdf",
                               "--optimize", "0", "-j", "8", "-l", attempt, str(upright), str(part)],
                              capture_output=True, text=True)
        if proc.returncode == 0:
            os.replace(part, dest)
            return dest
    return None


def _backup_page(path, index):
    return pymupdf.open(path)[index] if path else None


ORPHAN_PROMPT = """

补充任务：上一轮的分组漏掉了一些 OCR 行。用户这次发来的 JSON 中，existing 是已有分组及译文，orphans 是漏掉的行。
对每个漏掉的行：
- 如果它的内容已经包含在某个 existing 组的译文里（通常是同一段落的续行），把它的 id 放进 covered；
- 否则如果需要翻译，按同样规则输出新的组；
- 纯数字、手写签名、印章、logo（含与标志排在一起构成标识的机构名、院系名、品牌名）等不需要翻译的行，既不放进 covered 也不输出；拿不准时对照图片，属于标识的一律不输出。印刷的人名按上面的人名规则处理。
只输出 JSON：{"covered": [<id>, ...], "groups": [{"ids": [<id>, ...], "tr": "<译文>"}]}"""


def _nearest_group(lines: list[dict], groups: list[dict], i: int):
    """Index of the group with a line closest to line i (for covering it)."""
    a = lines[i]["bbox"]
    best, pick = None, None
    for n, g in enumerate(groups):
        for j in g["ids"]:
            b = lines[j]["bbox"]
            dist = max(a.y0 - b.y1, b.y0 - a.y1, 0) + max(a.x0 - b.x1, b.x0 - a.x1, 0)
            if best is None or dist < best:
                best, pick = dist, n
    return pick


def _system_prompt(job: Job, page: pymupdf.Page) -> str:
    prompt = SYSTEM.format(width=round(page.rect.width), height=round(page.rect.height),
                           source=LANG_NAMES.get(job.lang_in, job.lang_in),
                           target=LANG_NAMES.get(job.lang_out, job.lang_out))
    return prompt + job.glossary


def _chat(job: Job, client, system: str, payload, images: list[str], what: str, key: str,
          model: str = "") -> dict:
    """One JSON-answer model call with retries, quota fallback and cancellation."""
    last = None
    model = model or job.model
    for _ in range(4):
        _check(job)
        models = [m.strip() for m in model.split(",") if m.strip() and m.strip() not in job.exhausted]
        if not models:
            raise QuotaExhausted(str(last))
        try:
            text = payload if isinstance(payload, str) else json.dumps(payload, ensure_ascii=False)
            content = [{"type": "text", "text": text}]
            content += [{"type": "image_url", "image_url": {"url": f"data:image/jpeg;base64,{img}"}} for img in images]
            extra = {} if job.temperature is None else {"temperature": job.temperature}
            try:
                resp = client.chat.completions.create(model=models[0], messages=[
                    {"role": "system", "content": system}, {"role": "user", "content": content}], **extra)
            except Exception as exc:
                if not extra or "temperature" not in str(exc).lower():
                    raise
                job.temperature = None  # this model/endpoint only takes its default
                resp = client.chat.completions.create(model=models[0], messages=[
                    {"role": "system", "content": system}, {"role": "user", "content": content}])
            raw = re.sub(r"^```(?:json)?\s*|\s*```$", "", (resp.choices[0].message.content or "").strip())
            result = json.loads(raw)
            if not isinstance(result, dict) or key not in result:
                raise ValueError(f"no {key} in model answer")
            return result
        except Aborted:
            raise
        except Exception as exc:  # retry transient model/proxy errors
            last = exc
            if _quota_error(exc):
                job.exhausted.add(models[0])
    if last is not None and _quota_error(last):
        raise QuotaExhausted(str(last))
    raise RuntimeError(f"{what}: model call failed: {last}")


def _ask_model(job: Job, client, page: pymupdf.Page, image: str | None, payload, index: int, extra: str = "") -> dict:
    result = _chat(job, client, _system_prompt(job, page) + extra, payload, [image] if image else [],
                   f"page {index + 1}", "groups")
    if not isinstance(result["groups"], list):
        raise RuntimeError(f"page {index + 1}: malformed model answer")
    return result


# The model endpoint cuts images into 32x32 px patches and rejects more than 30000
# (a 1203x855 mm drawing at 144 dpi is 32528). Capping each image at ~4 MP keeps a
# review's two images far below that; A4/A3 pages stay at full dpi. OCR still runs
# on the full-resolution page, so only the model's view of tiny text gets softer.
MAX_IMAGE_PIXELS = 4_000_000


def _image_zoom(page: pymupdf.Page, dpi: float) -> float:
    zoom = dpi / 72
    pixels = page.rect.width * zoom * page.rect.height * zoom
    if pixels > MAX_IMAGE_PIXELS:
        zoom *= (MAX_IMAGE_PIXELS / pixels) ** 0.5 * 0.999  # stay under after rounding up
    return zoom


def _jpeg(page: pymupdf.Page, dpi: int = 144) -> str:
    zoom = _image_zoom(page, dpi)
    if zoom == dpi / 72:  # normal page: rendered exactly as before
        pix = page.get_pixmap(dpi=dpi)
    else:
        pix = page.get_pixmap(matrix=pymupdf.Matrix(zoom, zoom))
    return base64.b64encode(pix.tobytes("jpeg", jpg_quality=85)).decode()


def translate_page(job: Job, client, upright_path: Path, ocr_path: Path, index: int, tess: Path | None = None,
                   feedback: list[str] | None = None) -> dict:
    cache = job.workdir / "cache" / f"{index + 1:04d}.json"
    if cache.exists():
        try:
            return json.loads(cache.read_text())
        except ValueError:
            cache.unlink()  # damaged by a power cut: translate the page again
    ocr = pymupdf.open(ocr_path)
    lines = ocr_lines(ocr[index], _backup_page(tess, index))
    if not lines:
        result = {"groups": []}
    else:
        page = pymupdf.open(upright_path)[index]
        image = _jpeg(page)
        items = [{"id": i, "text": l["text"], "box": [round(v) for v in l["bbox"]]} for i, l in enumerate(lines)]
        shared = _shared_path(job, "pages", items, round(page.rect.width), round(page.rect.height),
                              SYSTEM, ORPHAN_PROMPT, job.glossary)
        # A redo after a failed quality check must not get the rejected answer back.
        cached = None if feedback else _read_shared(shared)
        if cached is not None:
            write_atomic(cache, json.dumps(cached, ensure_ascii=False))
            return cached
        redo = ""
        if feedback:
            redo = "\n\n上一版译文质检发现以下问题，这次务必避免：\n" + "\n".join("- " + f for f in feedback)
        result = _ask_model(job, client, page, image, items, index, redo)
        groups = [g for g in result["groups"] if isinstance(g, dict)]
        for g in groups:
            g["ids"] = [i for i in g.get("ids", []) if isinstance(i, int) and 0 <= i < len(lines)]
        taken = {i for g in groups for i in g["ids"]}
        # Lines the model deliberately left alone (logos etc.) are neither orphans nor leftovers.
        keep = sorted({i for i in result.get("keep") or [] if isinstance(i, int) and 0 <= i < len(lines)} - taken)
        orphans = [i for i in range(len(lines)) if i not in taken and i not in keep
                   and not re.match(r"(page|p\.)\s*\d+\W*$", lines[i]["text"], re.I)]
        if orphans and groups:  # the model skipped lines: ask (text only) whether they are covered
            extra = _ask_model(job, client, page, image, {
                "existing": [{"ids": g["ids"], "tr": g.get("tr") or g.get("zh") or ""} for g in groups],
                "orphans": [items[i] for i in orphans]}, index, ORPHAN_PROMPT)
            for i in extra.get("covered") or []:
                if isinstance(i, int) and i in orphans and i not in taken:
                    n = _nearest_group(lines, groups, i)
                    if n is not None:
                        groups[n]["ids"] = sorted(groups[n]["ids"] + [i], key=lambda k: lines[k]["bbox"].y0)
                        taken.add(i)
            for g in extra["groups"]:
                ids = [i for i in g.get("ids", []) if isinstance(i, int) and i in orphans and i not in taken]
                if ids and (g.get("tr") or g.get("zh")):
                    taken.update(ids)
                    groups.append({"ids": ids, "tr": g.get("tr") or g.get("zh")})
        result = {"groups": groups, "keep": keep}
        _write_shared(shared, result)
    write_atomic(cache, json.dumps(result, ensure_ascii=False))
    return result


# ---------------------------------------------------------------- rendering
def _colours(pix: pymupdf.Pixmap, rect: pymupdf.Rect):
    """(background, text) RGB 0-1 sampled around / inside rect."""
    W, H = pix.width, pix.height

    def px(x, y):
        return pix.pixel(min(max(int(x * SCALE), 0), W - 1), min(max(int(y * SCALE), 0), H - 1))[:3]

    ring = []
    for t in range(21):
        f = t / 20
        x, y = rect.x0 + f * rect.width, rect.y0 + f * rect.height
        ring += [px(x, rect.y0 - 2), px(x, rect.y1 + 2), px(rect.x0 - 2, y), px(rect.x1 + 2, y)]
    inside = [px(rect.x0 + (i + .5) * rect.width / 40, rect.y0 + (j + .5) * rect.height / 8)
              for i in range(40) for j in range(8)]

    def dominant(samples):
        # The most common colour, not the median: table borders would darken a median.
        buckets = collections.defaultdict(list)
        for c in samples:
            buckets[tuple(v // 24 for v in c)].append(c)
        common = max(buckets.values(), key=len)
        return tuple(statistics.median(c[i] for c in common) for i in range(3)), len(common) / len(samples)

    # Text strokes cover a minority of a line's box, so the colour filling most of it is
    # the background the text sits on, e.g. a coloured badge or button whose surroundings
    # (what the ring sees) are white. On busy or graded backgrounds no colour dominates
    # the inside and the surrounding ring decides.
    bg, share = dominant(inside)
    if share < 0.5:
        bg, _ = dominant(ring)
    dist = lambda c: sum((c[i] - bg[i]) ** 2 for i in range(3))  # noqa: E731
    far = sorted(inside, key=dist)[-max(8, len(inside) // 10):]
    fg = tuple(statistics.median(c[i] for c in far) for i in range(3))
    if dist(fg) < 60 ** 2:
        lum = 0.299 * bg[0] + 0.587 * bg[1] + 0.114 * bg[2]
        fg = (0, 0, 0) if lum > 140 else (255, 255, 255)
    return tuple(c / 255 for c in bg), tuple(c / 255 for c in fg)


def render_translation(doc: pymupdf.Document, index: int, lines, result, archive, font: str):
    """Return (one-page PDF with the translation, per-group layout info for checks)."""
    out = pymupdf.open()
    out.insert_pdf(doc, from_page=index, to_page=index)
    page = out[0]
    pix = doc[index].get_pixmap(dpi=DPI)
    used, jobs, info_ids, info = set(), [], [], []
    typical = statistics.median(l["bbox"].height for l in lines) if lines else 10
    for g in result.get("groups", []):
        try:
            ids = [int(i) for i in g.get("ids", [])]
        except (TypeError, ValueError):
            continue
        ids = [i for i in ids if 0 <= i < len(lines) and i not in used]
        text = str(g.get("tr") or g.get("zh") or "").strip()
        if not ids or not text:
            continue
        used.update(ids)
        cover = [cover_box(lines[i], text, typical) for i in ids]
        rect = pymupdf.Rect(cover[0][0])
        for box, _ in cover[1:]:
            rect |= box
        est = []
        for i, (box, src) in zip(ids, cover, strict=True):
            by_height = 1.1 * box.height
            by_width = 1.15 * box.width / (0.5 * len(src)) if len(src) >= 6 else by_height
            if box != lines[i]["bbox"] and box.height >= 1.6 * typical:
                est.append(min(by_height, by_width))  # a label beside a tall formula sign
            # single lines (headings, table cells): OCR height varies, keep at least body size
            elif len(ids) == 1:
                est.append(max(0.95 * by_height, 1.05 * typical))
            else:
                est.append(min(by_height, by_width))
        jobs.append((rect, statistics.median(lines[i]["bbox"].height for i in ids),
                     statistics.median(est), text, [box for box, _ in cover]))
        info_ids.append(ids)
    if jobs:
        body = [j[2] for j in jobs if j[1] < 1.5 * typical]
        if body:
            cap = 1.15 * statistics.median(body)
            jobs = [(r, lh, min(fs, cap) if lh < 1.5 * typical else fs, t, b) for r, lh, fs, t, b in jobs]
    for rect, lh, fs, text, boxes in jobs:  # cover first so later text is never painted over
        bg, _ = _colours(pix, rect)
        for b in boxes:
            pad_x, pad_y = 0.12 * lh, 0.15 * lh  # tight, so table borders survive
            page.draw_rect(pymupdf.Rect(b.x0 - pad_x, b.y0 - pad_y, b.x1 + pad_x, b.y1 + pad_y),
                           color=None, fill=bg, overlay=True)
    for n, (rect, lh, fs, text, boxes) in enumerate(jobs):
        _, fg = _colours(pix, rect)
        bold = "bold" if fs > 14 else "normal"
        css = (f"@font-face {{font-family: f; src: url({font}-Regular.ttf);}}"
               f"@font-face {{font-family: f; src: url({font}-Bold.ttf); font-weight: bold;}}"
               f"* {{font-family: f; font-size: {fs:.1f}px; line-height: 1.2; color: "
               f"#{''.join('%02x' % int(c * 255) for c in fg)}; font-weight: {bold}; margin: 0; padding: 0;}}")
        box = pymupdf.Rect(rect.x0, rect.y0 - 0.2 * lh, rect.x1 + 0.1 * rect.width,
                           max(rect.y1 + 0.3 * lh, rect.y0 - 0.2 * lh + 1.35 * fs))
        html = text.replace("&", "&amp;").replace("<", "&lt;").replace("\n", "<br>")
        _, scale = page.insert_htmlbox(box, html, css=css, archive=archive, scale_low=0)
        info.append({"ids": info_ids[n], "fs": round(fs * scale, 1), "scale": round(scale, 2)})
    out.subset_fonts()  # every insert_htmlbox embeds the whole CJK font
    return out, info


def _render_chunk(task):
    upright, ocr_path, tess, cache_dir, pages, lang_out, dual_dest, mono_dest = task
    doc, ocr = pymupdf.open(upright), pymupdf.open(ocr_path)
    archive = pymupdf.Archive(str(FONT_DIR))
    font = FONTS.get(lang_out, "SourceHanSansCN")
    dual, mono = pymupdf.open(), pymupdf.open()
    for i in pages:
        result = json.loads((Path(cache_dir) / f"{i + 1:04d}.json").read_text())
        translated, info = render_translation(doc, i, ocr_lines(ocr[i], _backup_page(tess, i)), result, archive, font)
        write_atomic(Path(cache_dir) / f"{i + 1:04d}.render.json", json.dumps(info))
        w, h = doc[i].rect.width, doc[i].rect.height
        if dual_dest:
            page = dual.new_page(width=w * 2, height=h)
            page.show_pdf_page(pymupdf.Rect(0, 0, w, h), doc, i)
            page.show_pdf_page(pymupdf.Rect(w, 0, w * 2, h), translated, 0)
        if mono_dest:
            mono.insert_pdf(translated)
        translated.close()
    for out, dest in ((dual, dual_dest), (mono, mono_dest)):
        if dest:
            out.save(dest + ".tmp", garbage=4, deflate=True)
            os.replace(dest + ".tmp", dest)
    return pages


# ocrmypdf --force-ocr rasterizes every page as a lossless image at up to 400 dpi, so a
# 0.3 MB scan came back as a 30 MB result that took minutes to upload. OCR and review
# keep the full images; only the delivered PDF is recompressed to JPEG, never below the
# customer's own scan: the target is the source's resolution, kept within
# RESULT_DPI_RANGE. MuPDF halves an image while it is at least twice the target, so a
# 400 dpi page of a 71 dpi scan ends at 200 dpi and a 300 dpi scan stays at 300 dpi.
# The translation is vector text, so this only touches the scanned background.
RESULT_DPI_RANGE = (150, 300)
RESULT_JPEG_QUALITY = 85


def source_dpi(path: Path) -> int:
    """Resolution of the sharpest scanned page in path, within RESULT_DPI_RANGE.

    Only images covering at least a third of the page count; a document with none
    (vector pages, which OCR rasterizes) gets the top of the range.
    """
    low, high = RESULT_DPI_RANGE
    best = 0.0
    with pymupdf.open(path) as doc:
        for page in doc:
            area = page.rect.width * page.rect.height
            for info in page.get_image_info():
                box = pymupdf.Rect(info["bbox"]) & page.rect
                if area <= 0 or box.width * box.height < area / 3 or box.width <= 0:
                    continue
                best = max(best, info["width"] / (box.width / 72), info["height"] / (box.height / 72))
    return int(min(max(best or high, low), high))


# ---------------------------------------------------------------- long pages
# E-commerce detail images and phone screenshots come as one page many times taller
# than wide (461 x 7800 pt). Apple OCR finds almost nothing on such a page, and the
# model sees it squeezed under MAX_IMAGE_PIXELS at ~76 dpi, so lines are missed and
# translations land on the wrong lines. Such pages are cut into tiles of about
# TILE_RATIO x their width, at the quietest row near each cut so text lines stay
# whole; every tile is OCR'd, translated and laid out as a page of its own, and the
# results are stacked back into the original page.
LONG_PAGE_RATIO = 2.5
TILE_RATIO = 1.4


def _row_busyness(page: pymupdf.Page, dpi: int = 36) -> tuple[list[float], float]:
    """Per pixel row: how much the row varies (0 for blank or flat rows); and pt per row."""
    pix = page.get_pixmap(dpi=dpi, colorspace=pymupdf.csGRAY, alpha=False)
    data, w = pix.samples, pix.width
    rows = []
    for y in range(pix.height):
        row = data[y * pix.stride:y * pix.stride + w]
        mean = sum(row) / w
        rows.append((sum((v - mean) ** 2 for v in row) / w) ** 0.5)
    smooth = [statistics.fmean(rows[max(0, y - 1):y + 2]) for y in range(len(rows))]
    return smooth, page.rect.height / pix.height


def tile_cuts(page: pymupdf.Page) -> list[float]:
    """Cut positions (pt) splitting a long page into tiles; empty for ordinary pages."""
    width, height = page.rect.width, page.rect.height
    if page.rotation % 360 or height <= LONG_PAGE_RATIO * width:
        return []
    busy, step = _row_busyness(page)
    target = TILE_RATIO * width
    cuts, last = [], 0.0
    while height - last > 1.25 * target:
        lo = int((last + 0.75 * target) / step)
        hi = min(int((last + 1.25 * target) / step), len(busy) - 1)
        aim = (last + target) / step
        # Prefer the widest blank band (the gap between blocks, not between two lines of
        # one paragraph) and cut through its middle; with no blank row, the quietest row.
        quiet = max(2.0, min(busy[lo:hi + 1]) + 1.0)
        bands, start = [], None
        for y in range(lo, hi + 2):
            if y <= hi and busy[y] <= quiet:
                start = y if start is None else start
            elif start is not None:
                bands.append((start, y - 1))
                start = None
        a, b = max(bands, key=lambda band: (band[1] - band[0], -abs((band[0] + band[1]) / 2 - aim)))
        last = (a + b) / 2 * step
        cuts.append(last)
    return cuts


def split_long_pages(job: Job) -> list[list[float]] | None:
    """Write job.workdir/tiles.pdf; returns [page, y0, y1] per tile, or None if no page is long."""
    with pymupdf.open(job.source) as src:
        cuts = [tile_cuts(page) for page in src]
        if not any(cuts):
            return None
        tiles, layout = pymupdf.open(), []
        for page, page_cuts in zip(src, cuts, strict=True):
            edges = [0.0, *page_cuts, page.rect.height]
            for y0, y1 in zip(edges, edges[1:]):
                tile = tiles.new_page(width=page.rect.width, height=y1 - y0)
                tile.show_pdf_page(tile.rect, src, page.number,
                                   clip=pymupdf.Rect(0, y0, page.rect.width, y1))
                layout.append([page.number, y0, y1])
    tiles.save(job.workdir / "tiles.pdf", garbage=3, deflate=True)
    return layout


def stitch_tiles(path: Path, layout: list[list[float]]):
    """Stack the translated tiles in path back into the original pages, in place."""
    out = pymupdf.open()
    with pymupdf.open(path) as tiles:
        page = None
        for n, (index, y0, y1) in enumerate(layout):
            width = tiles[n].rect.width  # double for the dual layout
            if y0 == 0:
                height = next(b for i, _, b in reversed(layout) if i == index)
                page = out.new_page(width=width, height=height)
            page.show_pdf_page(pymupdf.Rect(0, y0, width, y1), tiles, n)
        out.set_metadata(tiles.metadata)
        out.save(path.with_suffix(".stitched.pdf"), garbage=4, deflate=True)
    os.replace(path.with_suffix(".stitched.pdf"), path)


def _merge(parts: list[Path], dest: Path, dpi: int = RESULT_DPI_RANGE[1]):
    out = pymupdf.open()
    for part in parts:
        with pymupdf.open(part) as doc:
            out.insert_pdf(doc)
    out.rewrite_images(dpi_threshold=dpi + 1, dpi_target=dpi, quality=RESULT_JPEG_QUALITY)
    out.set_metadata({"producer": "pdfmath scan worker", "title": "translated"})
    out.save(dest, garbage=4, deflate=True)


# ---------------------------------------------------------------- quality control
TERMS_PROMPT = """你是术语专家。用户发来一份{source}文档的 OCR 文本（可能有识别错字）。
请找出需要在全文统一翻译的专业术语、专有名词、机构/公司名、合同定义词（如带引号或首字母大写的定义）和反复出现的固定说法，最多 80 个，给出规范的{target}译法。
""" + NAME_RULE + """地名按惯用译名，缩写保留原样。只输出 JSON：{{"terms": [{{"src": "原文", "tgt": "译文"}}, ...]}}"""

REVIEW_PROMPT = """你是排版与翻译质检员。第一张图是扫描文档的原页，第二张是把原文替换成{target}后的译文页（尽量保留原页面样式）。
逐项检查译文页：
1. 是否残留未翻译的{source}正文（页眉页脚、手写签名、纯数字、代码除外；logo、印章及与标志排在一起构成标识的机构名保持原文是正确的）；印刷的人名按此规则检查：""" + NAME_RULE + """
2. 译文或覆盖色块是否遮挡了图形、签章、logo、表格线或数字（logo 被改写或被译文盖住算严重）；
3. 文字是否重叠、溢出边界或明显错位、过小难以辨认；
4. 数字、金额、日期、期限、条款编号是否与原页一致；
5. 是否有明显漏译或意思相反的误译。
判断标准：
- 只有读者会被误导、信息缺失或明显看不清的才是“严重”；轻微的字体/字号/颜色差异、表格线细小断点、个别词译法可商榷都是“轻微”。
- 原页本身就有的情况（编号缺失、错字、模糊）不算问题。
只输出 JSON：{{"ok": 没有严重问题时为 true, "issues": [{{"severity": "严重" 或 "轻微", "desc": "问题（指明位置）"}}, ...]}}"""

def extract_terms(job: Job, client, ocr: Path, tess: Path | None) -> int:
    """Add a document-wide glossary to the prompt; returns the number of terms."""
    cache = job.workdir / "terms.json"
    if cache.exists():
        terms = json.loads(cache.read_text())
    else:
        doc = pymupdf.open(ocr)
        texts = [" ".join(l["text"] for l in ocr_lines(doc[i], _backup_page(tess, i))) for i in range(len(doc))]
        budget, picked = 60000, []
        step = max(1, int(sum(len(t) for t in texts) / budget) + 1)  # sample evenly when long
        for i in range(0, len(texts), step):
            picked.append(f"[第{i + 1}页] {texts[i]}")
        text = "\n".join(picked)[:budget]
        prompt = TERMS_PROMPT.format(source=LANG_NAMES.get(job.lang_in, job.lang_in),
                                     target=LANG_NAMES.get(job.lang_out, job.lang_out))
        shared = _shared_path(job, "terms", text, prompt, job.glossary)
        terms = _read_shared(shared)
        if terms is None:
            answer = _chat(job, client, prompt, text, [], "glossary", "terms", job.review_model)
            terms = [t for t in answer["terms"] if isinstance(t, dict) and t.get("src") and t.get("tgt")][:80]
            _write_shared(shared, terms)
        write_atomic(cache, json.dumps(terms, ensure_ascii=False))
    if terms:
        job.glossary += "\n\n术语表（全文统一使用以下译法，大小写和单复数变化同理）：\n" + \
            "\n".join(f"{t['src']} = {t['tgt']}" for t in terms)
    return len(terms)


LEFTOVER = "残留原文："  # rule finding that triggers the light check's review
CJK_SOURCES = {"zh", "ja", "ko"}
# Han, kana and hangul: these scripts write words without spaces.
_CJK = re.compile(r"[぀-ヿ㐀-鿿가-힯]")


def _looks_like_prose(text: str, lang_in: str) -> bool:
    """A line that is real untranslated source text, not a code, number or label."""
    if lang_in in CJK_SOURCES:  # one unspaced sentence is a single "word"; count characters
        return len(_CJK.findall(text)) >= 4
    return len(re.findall(r"[^\W\d_]{3,}", text)) >= 3


def rule_issues(lines: list[dict], result: dict, info: list[dict], lang_in: str = "") -> list[str]:
    """Cheap checks: leftover source sentences and squeezed text.

    Numbers are left to the vision review: OCR misreads digits the model corrects
    from the image, so comparing OCR digits with the translation gives false alarms.
    """
    issues = []
    taken = {i for g in result.get("groups", []) for i in g.get("ids", [])} | set(result.get("keep") or [])
    left = [lines[i]["text"] for i in range(len(lines)) if i not in taken
            and _looks_like_prose(lines[i]["text"], lang_in)
            and not re.search(r"www\.|https?:|@|\.(com|org|net|gov)\b", lines[i]["text"], re.I)]
    if left:
        issues.append(LEFTOVER + "；".join(t[:40] for t in left[:3]))
    for item in info:
        if item["fs"] < 5.5 or item["scale"] < 0.7:
            text = " ".join(lines[i]["text"] for i in item["ids"] if 0 <= i < len(lines))
            issues.append(f"译文过挤或字号过小（{item['fs']}pt）：{text[:40]}")
    return issues


def review_page(job: Job, client, original: pymupdf.Page, translated: pymupdf.Page) -> list[str]:
    prompt = REVIEW_PROMPT.format(source=LANG_NAMES.get(job.lang_in, job.lang_in),
                                  target=LANG_NAMES.get(job.lang_out, job.lang_out))
    answer = _chat(job, client, prompt, "请质检这一页。", [_jpeg(original, 110), _jpeg(translated, 110)],
                   "review", "ok", job.review_model)
    serious = []
    for item in answer.get("issues") or []:
        if isinstance(item, dict):
            if item.get("severity") == "严重":
                serious.append(str(item.get("desc", ""))[:120])
        elif answer.get("ok") is not True:
            serious.append(str(item)[:120])
    if not serious and answer.get("ok") is False:
        serious.append("质检未通过")
    return serious[:5]


# ---------------------------------------------------------------- entry point
def load_glossary(path: str | None) -> str:
    if not path or not Path(path).exists():
        return ""
    import csv
    rows = [r for r in csv.reader(open(path, encoding="utf-8"))][1:]
    table = "\n".join(f"{r[0]} = {r[1]}" for r in rows if len(r) >= 2)
    return "\n\n术语表（必须严格按此翻译）：\n" + table if table else ""


def _translate_all(job: Job, client, upright, ocr, tess, pages, lo, hi, feedback=None):
    done = 0
    with ThreadPoolExecutor(job.workers) as pool:
        futures = [pool.submit(translate_page, job, client, upright, ocr, i, tess, (feedback or {}).get(i))
                   for i in pages]
        try:
            for future in as_completed(futures):
                future.result()
                done += 1
                job.progress(f"翻译 {done}/{len(pages)} 页", lo + (hi - lo) * done / len(pages))
        except BaseException:
            for f in futures:
                f.cancel()
            raise


def _render(job: Job, upright, ocr, tess, total, chunk_ids, lo, hi, mono: bool = False, dual: bool = True):
    """Render chunks; `mono` adds mono pages for a review even when not delivered."""
    want_dual = dual and job.output in ("dual", "both")
    want_mono = job.output in ("mono", "both") or job.full_qa or mono  # mono pages are what gets reviewed
    chunks = [list(range(i, min(i + CHUNK, total))) for i in range(0, total, CHUNK)]
    tasks = [(str(upright), str(ocr), str(tess) if tess else "", str(job.workdir / "cache"), chunks[n],
              job.lang_out, str(job.workdir / f"dual_{n:04d}.pdf") if want_dual else "",
              str(job.workdir / f"mono_{n:04d}.pdf") if want_mono else "") for n in chunk_ids]
    with mp.get_context("spawn").Pool(job.render_procs, maxtasksperchild=1) as pool:
        for n, _ in enumerate(pool.imap_unordered(_render_chunk, tasks), 1):
            _check(job)
            job.progress("生成 PDF", lo + (hi - lo) * n / len(tasks))
    return len(chunks)


def _rule_hits(job: Job, ocr, tess, pages) -> dict[int, list[str]]:
    """Free rule checks (no model call) on rendered pages; only pages with findings."""
    ocr_doc, cache, hits = pymupdf.open(ocr), job.workdir / "cache", {}
    for i in pages:
        lines = ocr_lines(ocr_doc[i], _backup_page(tess, i))
        result = json.loads((cache / f"{i + 1:04d}.json").read_text())
        info = json.loads((cache / f"{i + 1:04d}.render.json").read_text())
        issues = rule_issues(lines, result, info, job.lang_in)
        if issues:
            hits[i] = issues
    return hits


def _quality_check(job: Job, client, upright, ocr, tess, pages, lo, hi, confirm: bool = False) -> dict[int, list[str]]:
    """Issues per page (rule checks + vision review), only pages with issues returned.

    With confirm, a page counts only when the vision review finds a serious issue;
    rule findings alone (often just tight text a redo cannot fix) do not.
    """
    orig, ocr_doc = pymupdf.open(upright), pymupdf.open(ocr)
    cache = job.workdir / "cache"

    def check(i):
        lines = ocr_lines(ocr_doc[i], _backup_page(tess, i))
        result = json.loads((cache / f"{i + 1:04d}.json").read_text())
        info = json.loads((cache / f"{i + 1:04d}.render.json").read_text())
        issues = rule_issues(lines, result, info, job.lang_in)
        with pymupdf.open(job.workdir / f"mono_{i // CHUNK:04d}.pdf") as mono:
            serious = review_page(job, client, orig[i], mono[i % CHUNK])
        if confirm and not serious:
            return i, []
        return i, issues + serious

    found, done = {}, 0
    with ThreadPoolExecutor(job.workers) as pool:
        for future in as_completed([pool.submit(check, i) for i in pages]):
            i, issues = future.result()
            if issues:
                found[i] = issues
            done += 1
            job.progress(f"质检 {done}/{len(pages)} 页", lo + (hi - lo) * done / len(pages))
    return found


def format_report(report: dict) -> str:
    if report.get("light"):
        lines = [f"自动质检：{report['pages']} 页规则检查，{report['checked']} 页可疑，"
                 f"复核确认 {report['redone']} 页已重做，最终 {len(report['problems'])} 页仍有问题"]
        if report.get("review_error"):
            lines.append("复核未完成，按首轮译文交付：" + report["review_error"][:120])
    else:
        lines = [f"术语表：统一 {report['terms']} 个术语",
                 f"质检：{report['pages']} 页，首轮 {report['redone']} 页有问题已重做，"
                 f"最终 {len(report['problems'])} 页仍有问题"]
    for item in report["problems"][:10]:
        lines.append(f"· {item.get('label') or '第 %d 页' % item['page']}：" + "；".join(item["issues"])[:150])
    if len(report["problems"]) > 10:
        lines.append(f"· 另有 {len(report['problems']) - 10} 页")
    return "\n".join(lines)


def process(job: Job, client) -> dict:
    """Translate job.source; returns {"dual": path, "mono": path} per job.output.

    With job.full_qa, a document-wide glossary is built first, every page is then
    checked (rules + vision review), pages with issues are translated again once with
    the issues as feedback, and job.report lists what still fails. Every other order
    gets the rule-gated light check (_light_quality_check).
    """
    job.workdir.mkdir(parents=True, exist_ok=True)
    (job.workdir / "cache").mkdir(exist_ok=True)
    if not FONT_DIR.joinpath("SourceHanSansCN-Regular.ttf").exists():
        raise RuntimeError(f"fonts missing in {FONT_DIR}")
    original = job.source
    layout = split_long_pages(job)
    if layout:  # from here on every tile is a page; stitched back at the end
        job.source = job.workdir / "tiles.pdf"
    job.progress("扶正页面", 2)
    upright, ocr = straighten(job)
    total = len(pymupdf.open(upright))
    _check(job)
    job.progress("补充识别漏掉的文字行", 18)
    tess = run_backup_ocr(job, upright)
    _check(job)

    qa = job.full_qa
    terms = 0
    if qa:
        job.progress("统一术语", 21)
        terms = extract_terms(job, client, ocr, tess)
    pages = list(range(total))
    _translate_all(job, client, upright, ocr, tess, pages, 25, 70 if qa else 85)
    _check(job)
    chunk_count = _render(job, upright, ocr, tess, total, range((total + CHUNK - 1) // CHUNK),
                          70 if qa else 86, 76 if qa else 98)

    if qa:
        found = _quality_check(job, client, upright, ocr, tess, pages, 76, 90)
        problems = found
        if found:
            job.progress(f"重做 {len(found)} 页", 90)
            for i in found:
                (job.workdir / "cache" / f"{i + 1:04d}.json").unlink(missing_ok=True)
            redo = sorted(found)
            _translate_all(job, client, upright, ocr, tess, redo, 90, 94, feedback=found)
            _render(job, upright, ocr, tess, total, sorted({i // CHUNK for i in redo}), 94, 96)
            problems = _quality_check(job, client, upright, ocr, tess, redo, 96, 98)
        job.report = {"pages": total, "terms": terms, "redone": len(found),
                      "problems": [{"page": i + 1, "issues": problems[i]} for i in sorted(problems)]}
    elif job.light_qa:
        _light_quality_check(job, client, upright, ocr, tess, total)

    if layout and job.report:  # quality control saw tiles; name them by page and part
        for item in job.report["problems"]:
            index = layout[item["page"] - 1][0]
            part = sum(1 for i, _, _ in layout[:item["page"]] if i == index)
            item["label"] = f"第 {index + 1} 页第 {part} 块"
    results = {}
    dpi = source_dpi(original)
    for kind in ("dual", "mono"):
        if job.output in (kind, "both"):
            dest = job.workdir / f"result-{kind}.pdf"
            _merge([job.workdir / f"{kind}_{n:04d}.pdf" for n in range(chunk_count)], dest, dpi)
            if layout:
                stitch_tiles(dest, layout)
            results[kind] = dest
    return results


def _light_quality_check(job: Job, client, upright, ocr, tess, total):
    """Quality control priced for every order: rules first, the model only where they hit.

    Only leftover source text triggers a review. Cramped text does not: zh->en pages
    are almost always shrunk to fit, and a retranslation cannot make English fit a
    Chinese-sized box (sampled 2026-11-17: 9 of 10 zh->en pages flagged, the redo
    cleared 1 of 4). Pages with leftovers get a vision review; pages the review
    confirms are translated again once with its findings and checked again. The
    review is optional: if it cannot run, the first translation is delivered.
    job.report stays empty when no page has leftovers.
    """
    hits = {i: issues for i, issues in _rule_hits(job, ocr, tess, range(total)).items()
            if any(issue.startswith(LEFTOVER) for issue in issues)}
    if not hits:
        return
    flagged = sorted(hits)
    report = {"light": True, "pages": total, "terms": 0, "checked": len(flagged), "redone": 0, "problems": []}
    job.report = report
    chunks = sorted({i // CHUNK for i in flagged})
    try:
        if job.output not in ("mono", "both"):  # the review looks at mono pages
            _render(job, upright, ocr, tess, total, chunks, 98, 98.3, mono=True, dual=False)
        found = _quality_check(job, client, upright, ocr, tess, flagged, 98.3, 98.6, confirm=True)
    except Aborted:
        raise
    except Exception as exc:  # incl. the review model's quota: never hold a finished order back
        report["review_error"] = f"{type(exc).__name__}: {exc}"[:200]
        return
    if not found:
        return
    job.progress(f"重做 {len(found)} 页", 98.6)
    for i in found:
        (job.workdir / "cache" / f"{i + 1:04d}.json").unlink(missing_ok=True)
    redo = sorted(found)
    _translate_all(job, client, upright, ocr, tess, redo, 98.6, 98.8, feedback=found)
    _render(job, upright, ocr, tess, total, sorted({i // CHUNK for i in redo}), 98.8, 99, mono=True)
    try:
        problems = _quality_check(job, client, upright, ocr, tess, redo, 99, 99.2, confirm=True)
    except Aborted:
        raise
    except Exception as exc:
        report["review_error"] = f"{type(exc).__name__}: {exc}"[:200]
        problems = {}
    report["redone"] = len(found)
    report["problems"] = [{"page": i + 1, "issues": problems[i]} for i in sorted(problems)]


def cleanup(workdir: Path):
    shutil.rmtree(workdir, ignore_errors=True)


if __name__ == "__main__":
    sys.exit("use scan_worker.py")
