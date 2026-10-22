#!/bin/bash
# PDF2ZH Python API service launcher (production or development).
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
export PDF2ZH_API_HOST="${PDF2ZH_API_HOST:-127.0.0.1}"

# --- tuning for an 8-core / 8G box ---
export PDF2ZH_MAX_CONCURRENT="${PDF2ZH_MAX_CONCURRENT:-2}"
export PDF2ZH_QPS="${PDF2ZH_QPS:-4}"
export PDF2ZH_POOL_MAX_WORKERS="${PDF2ZH_POOL_MAX_WORKERS:-4}"
export PDF2ZH_NO_MONO="${PDF2ZH_NO_MONO:-true}"
export PDF2ZH_MAX_PAGES_PER_PART="${PDF2ZH_MAX_PAGES_PER_PART:-50}"

cd "$WORKDIR"
exec pdf2zh --api
