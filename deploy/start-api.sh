#!/bin/bash
# PDF2ZH Python API service launcher (production deploy).
# Tracked in the repo; the server symlinks /root/pdf/start-api.sh here.
# All PDF2ZH_* tuning is read by THIS process, not the Go gateway.
set -u

# --- deploy-specific knobs (override via environment if the host differs) ---
CONDA_SH="${CONDA_SH:-/root/miniconda3/etc/profile.d/conda.sh}"
CONDA_ENV="${CONDA_ENV:-babledoc}"
WORKDIR="${WORKDIR:-/root}"

source "$CONDA_SH"
conda activate "$CONDA_ENV"

# --- bind ---
export PDF2ZH_API_HOST=127.0.0.1          # localhost only; the gateway proxies it

# --- tuning for an 8-core / 8G box ---
export PDF2ZH_MAX_CONCURRENT=3            # translation jobs running at once; the rest queue
export PDF2ZH_QPS=4                       # upstream requests/sec per job (x MAX_CONCURRENT in total)
export PDF2ZH_POOL_MAX_WORKERS=4          # per-job paragraph concurrency
export PDF2ZH_NO_MONO=true                # emit dual-language PDF only
export PDF2ZH_MAX_PAGES_PER_PART=50       # split docs >50 pages to cap peak memory

cd "$WORKDIR"
exec pdf2zh --api
