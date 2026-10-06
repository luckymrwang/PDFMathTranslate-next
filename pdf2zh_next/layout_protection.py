"""Preserve figure text using BabelDOC's protected, non-text layout regions."""

from babeldoc.docvision.base_doclayout import DocLayoutModel, YoloResult


class FigurePreservingLayoutModel(DocLayoutModel):
    """Wrap the detector without modifying its shared results or model names.

    BabelDOC considers figure_text translatable and creates fallback text lines
    inside figure regions. Its isolate_formula category precedes fallback lines
    and retains original characters, so use that protected category for detected
    figure regions. Captions and tables keep their original classifications.
    """

    protected_labels = {"figure", "image", "figure_text", "figure_text_hybrid", "chart"}

    def __init__(self, model):
        self.model = model

    @property
    def stride(self):
        return self.model.stride

    def handle_document(self, *args, **kwargs):
        for page, result in self.model.handle_document(*args, **kwargs):
            def protect(name):
                return "isolate_formula" if name in self.protected_labels else name

            names = (
                {key: protect(value) for key, value in result.names.items()}
                if isinstance(result.names, dict)
                else [protect(value) for value in result.names]
            )
            yield page, YoloResult(names=names, boxes=list(result.boxes))
