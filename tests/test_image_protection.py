from types import SimpleNamespace

from babeldoc.docvision.base_doclayout import YoloResult
from babeldoc.format.pdf.document_il import Box
from babeldoc.format.pdf.document_il.utils.layout_helper import (
    build_layout_index,
    get_character_layout,
    is_text_layout,
)

from pdf2zh_next.http_api import TranslateRequest, _build_settings
from pdf2zh_next.layout_protection import FigurePreservingLayoutModel


def test_api_defaults_to_preserving_images(tmp_path):
    settings = _build_settings(TranslateRequest(), tmp_path)
    assert settings.pdf.skip_image_translation is True
    assert settings.pdf.ocr_workaround is False
    assert settings.pdf.auto_enable_ocr_workaround is False


def test_figure_characters_stay_protected_even_with_fallback_text():
    names = {0: "figure_text", 1: "figure_caption", 2: "table_text"}
    result = YoloResult(names=names, boxes=[])

    class FakeDetector:
        stride = 32

        def handle_document(self, *args, **kwargs):
            yield "page", result

    model = FigurePreservingLayoutModel(FakeDetector())
    _, protected = next(model.handle_document())
    assert names[0] == "figure_text"  # No shared detector mutation.
    assert protected.names[1] == "figure_caption"
    assert protected.names[2] == "table_text"
    layouts = [
        SimpleNamespace(id=1, class_name=protected.names[0], box=Box(0, 0, 100, 100)),
        SimpleNamespace(id=2, class_name="fallback_line", box=Box(10, 10, 40, 20)),
    ]
    index, mapping = build_layout_index(SimpleNamespace(page_layout=layouts))
    char = SimpleNamespace(visual_bbox=SimpleNamespace(box=Box(12, 12, 18, 18)))
    selected = get_character_layout(char, index, mapping)
    assert selected.name == "isolate_formula"
    assert not is_text_layout(selected)
