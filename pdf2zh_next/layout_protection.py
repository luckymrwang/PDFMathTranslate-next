"""Preserve figure text using BabelDOC's protected, non-text layout regions."""

from babeldoc.docvision.base_doclayout import DocLayoutModel, YoloResult

# A detected figure whose area lies at least this much inside a table is treated
# as part of the table. Layout models often tag a whole table (invoices, quotes)
# as a low-confidence figure too; protecting it would leave the table untranslated.
FIGURE_ON_TABLE_OVERLAP = 0.5


def _overlap_ratio(inner, outer) -> float:
    """Share of `inner`'s area that falls inside `outer` (both xyxy)."""
    ix0, iy0, ix1, iy1 = (float(v) for v in inner)
    ox0, oy0, ox1, oy1 = (float(v) for v in outer)
    width = min(ix1, ox1) - max(ix0, ox0)
    height = min(iy1, oy1) - max(iy0, oy0)
    area = (ix1 - ix0) * (iy1 - iy0)
    if width <= 0 or height <= 0 or area <= 0:
        return 0.0
    return width * height / area


class FigurePreservingLayoutModel(DocLayoutModel):
    """Wrap the detector without modifying its shared results or model names.

    BabelDOC considers figure_text translatable and creates fallback text lines
    inside figure regions. Its isolate_formula category precedes fallback lines
    and retains original characters, so use that protected category for detected
    figure regions. Captions and tables keep their original classifications.

    A figure that mostly sits on a detected table is dropped instead: the table's
    text must still be translated.
    """

    protected_labels = {"figure", "image", "figure_text", "figure_text_hybrid", "chart"}

    def __init__(self, model):
        self.model = model

    @property
    def stride(self):
        return self.model.stride

    def _drop_figures_on_tables(self, result):
        names = result.names
        boxes = list(result.boxes)
        tables = [b.xyxy for b in boxes if names[int(b.cls)] == "table"]
        if not tables:
            return boxes
        return [
            b
            for b in boxes
            if names[int(b.cls)] not in self.protected_labels
            or not any(
                _overlap_ratio(b.xyxy, table) >= FIGURE_ON_TABLE_OVERLAP
                for table in tables
            )
        ]

    def handle_document(self, *args, **kwargs):
        for page, result in self.model.handle_document(*args, **kwargs):
            def protect(name):
                return "isolate_formula" if name in self.protected_labels else name

            names = (
                {key: protect(value) for key, value in result.names.items()}
                if isinstance(result.names, dict)
                else [protect(value) for value in result.names]
            )
            yield page, YoloResult(names=names, boxes=self._drop_figures_on_tables(result))
