from babeldoc.docvision.base_doclayout import YoloBox
from babeldoc.docvision.base_doclayout import YoloResult
from pdf2zh_next.http_api import TranslateRequest
from pdf2zh_next.http_api import _build_settings
from pdf2zh_next.layout_protection import FigurePreservingLayoutModel


def test_api_defaults_to_preserving_images(tmp_path):
    settings = _build_settings(TranslateRequest(), tmp_path)
    assert settings.pdf.skip_image_translation is True
    assert settings.pdf.ocr_workaround is False
    assert settings.pdf.auto_enable_ocr_workaround is False


def test_figure_labels_become_protected_without_mutating_the_detector():
    names = {0: "figure_text", 1: "figure_caption", 2: "table_text"}
    result = YoloResult(names=names, boxes=[])

    class FakeDetector:
        stride = 32

        def handle_document(self, *args, **kwargs):
            yield "page", result

    model = FigurePreservingLayoutModel(FakeDetector())
    _, protected = next(model.handle_document())
    assert names[0] == "figure_text"  # No shared detector mutation.
    assert protected.names[0] == "isolate_formula"
    assert protected.names[1] == "figure_caption"
    assert protected.names[2] == "table_text"


BODY = "This paragraph is ordinary body text outside the figure."
FIGURE = "Axis label printed inside the chart area."


def _sent_to_translator(tmp_path, protect):
    """Run BabelDOC on one page whose lower text sits in a detected figure.

    BabelDOC 0.7 assigns characters to layout regions in Rust, so the only way
    to check protection is a real run with a translator that records its input.
    """
    import numpy as np
    import pymupdf
    from babeldoc.format.pdf.high_level import translate
    from babeldoc.format.pdf.translation_config import TranslationConfig
    from babeldoc.format.pdf.translation_config import WatermarkOutputMode
    from babeldoc.translator.translator import BaseTranslator

    pdf = tmp_path / "figure.pdf"
    doc = pymupdf.open()
    page = doc.new_page(width=595, height=842)
    page.insert_text((60, 110), BODY, fontname="helv", fontsize=12)
    page.draw_rect(pymupdf.Rect(50, 280, 545, 420), color=(0, 0, 0))
    page.insert_text((70, 350), FIGURE, fontname="helv", fontsize=12)
    doc.save(pdf)

    class Detector:
        stride = 32

        def handle_document(self, pages, *args, **kwargs):
            for page in pages:
                # Boxes are numpy values in PDF points, origin top-left, like the
                # real model's (BabelDOC calls .item() on them).
                yield page, YoloResult(
                    names={0: "plain text", 1: "figure"},
                    boxes=[
                        YoloBox(xyxy=np.array(xyxy, dtype=np.float32), conf=np.float32(0.9), cls=np.float32(cls))
                        for xyxy, cls in (((50, 90, 545, 125), 0), ((50, 280, 545, 420), 1))
                    ],
                )

    sent = []

    class Recorder(BaseTranslator):
        name = "recorder"

        def do_translate(self, text, rate_limit_params=None):
            sent.append(text)
            return "译" + text

        def do_llm_translate(self, text, rate_limit_params=None):
            raise NotImplementedError

    layout = FigurePreservingLayoutModel(Detector()) if protect else Detector()
    translate(
        TranslationConfig(
            input_file=str(pdf),
            font=None,
            translator=Recorder("en", "zh", True),
            lang_in="en",
            lang_out="zh",
            output_dir=str(tmp_path / "out"),
            working_dir=str(tmp_path / "work"),
            doc_layout_model=layout,
            no_dual=True,
            skip_scanned_detection=True,
            auto_extract_glossary=False,
            watermark_output_mode=WatermarkOutputMode.NoWatermark,
        )
    )
    return " ".join(sent)


def test_text_inside_a_detected_figure_is_not_translated(tmp_path):
    sent = _sent_to_translator(tmp_path, protect=True)
    assert "ordinary body text" in sent
    assert "inside the chart" not in sent


def test_figure_text_is_translated_without_protection(tmp_path):
    # Control: proves the wrapper, not BabelDOC itself, keeps figure text.
    sent = _sent_to_translator(tmp_path, protect=False)
    assert "ordinary body text" in sent
    assert "inside the chart" in sent


def _protected_boxes(names, boxes):
    class Detector:
        stride = 32

        def handle_document(self, *args, **kwargs):
            yield "page", YoloResult(
                names=names,
                boxes=[YoloBox(xyxy=xyxy, conf=conf, cls=cls) for xyxy, conf, cls in boxes],
            )

    _, result = next(FigurePreservingLayoutModel(Detector()).handle_document())
    return [(tuple(b.xyxy), result.names[int(b.cls)]) for b in result.boxes]


def test_figure_laid_over_a_table_does_not_hide_the_table():
    # An invoice: the detector tags the whole table as a low-confidence figure too.
    names = {0: "table", 1: "figure"}
    kept = _protected_boxes(
        names, [((30, 230, 570, 780), 0.69, 0), ((32, 233, 565, 781), 0.36, 1)]
    )
    assert kept == [((30, 230, 570, 780), "table")]


def test_real_figure_stays_protected_next_to_a_table():
    names = {0: "table", 1: "figure"}
    kept = _protected_boxes(
        names, [((30, 400, 570, 780), 0.8, 0), ((100, 40, 400, 300), 0.7, 1)]
    )
    assert ((100, 40, 400, 300), "isolate_formula") in kept
    assert ((30, 400, 570, 780), "table") in kept


def test_figure_only_touching_a_table_edge_stays_protected():
    names = {0: "table", 1: "figure"}
    kept = _protected_boxes(
        names, [((30, 400, 570, 780), 0.8, 0), ((100, 300, 400, 420), 0.7, 1)]
    )
    assert ((100, 300, 400, 420), "isolate_formula") in kept
