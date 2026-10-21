FROM ghcr.io/astral-sh/uv:python3.13-bookworm-slim

WORKDIR /app


# 7860: Gradio WebUI (--gui)  |  11008: HTTP API (--api)
EXPOSE 7860 11008

ENV PYTHONUNBUFFERED=1

# # Download all required fonts
# ADD "https://github.com/satbyy/go-noto-universal/releases/download/v7.0/GoNotoKurrent-Regular.ttf" /app/
# ADD "https://github.com/timelic/source-han-serif/releases/download/main/SourceHanSerifCN-Regular.ttf" /app/
# ADD "https://github.com/timelic/source-han-serif/releases/download/main/SourceHanSerifTW-Regular.ttf" /app/
# ADD "https://github.com/timelic/source-han-serif/releases/download/main/SourceHanSerifJP-Regular.ttf" /app/
# ADD "https://github.com/timelic/source-han-serif/releases/download/main/SourceHanSerifKR-Regular.ttf" /app/

RUN apt-get update && \
     apt-get install --no-install-recommends -y libgl1 libglib2.0-0 libxext6 libsm6 libxrender1 build-essential && \
     rm -rf /var/lib/apt/lists/*

COPY pyproject.toml .
RUN uv pip install --system --no-cache -r pyproject.toml && babeldoc --version && babeldoc --warmup

COPY . .

# Calls for a random number to break the cahing of babeldoc upgrade
# (https://stackoverflow.com/questions/35134713/disable-cache-for-specific-run-commands/58801213#58801213)
ADD "https://www.random.org/cgi-bin/randbyte?nbytes=10&format=h" skipcache

# bitstring<5: bitstring 5.0 removed BitStream, which babeldoc still imports
# (babeldoc declares bitstring>=4.3.0 with no upper bound).
RUN uv pip install --system --no-cache . && uv pip install --system --no-cache --compile-bytecode -U babeldoc "pymupdf<1.25.3" "bitstring<5" && babeldoc --version && babeldoc --warmup
RUN pdf2zh --version
# Default: Gradio WebUI. For the translation HTTP API service, override at runtime:
#   docker run -p 11008:11008 <image> pdf2zh --api
CMD ["pdf2zh", "--gui"]
