"""interest-memory installer — provider presets.

Provider catalogue for the guided installer. LLM/embedding presets carry a
base URL (OpenAI-compatible), the env var that holds the API key, and a
fallback flash-tier default model used when the live /models query fails.
Model lists are NOT hardcoded: they are fetched at install time via the
provider's models endpoint (see fetch_models), so catalogues stay current.

Model discovery mirrors Hermes' approach (hermes_cli/web_server.py): a
per-key-env probe map carrying each provider's models endpoint + auth style
("bearer" header vs "query" ?key=), with a tolerant response parser.
"""
from __future__ import annotations

import os
from dataclasses import dataclass

# ---------------------------------------------------------------------------
# Provider model
# ---------------------------------------------------------------------------
@dataclass(frozen=True)
class Provider:
    name: str
    url: str                # OpenAI-compatible base URL (usually includes /v1)
    key_env: str | None     # env var holding the API key; None for keyless/local
    default_model: str      # fallback when the /models query fails
    dims: int = 0           # embedding dimensionality (0 = not applicable)
    models_path: str = "/models"  # endpoint path (absolute URL if starting with http)
    auth: str = "bearer"    # "bearer" (Authorization) or "query" (?key=)


# ---------------------------------------------------------------------------
# LLM presets (base URLs / key envs verified against models.dev catalogue).
# OpenCode Go is the default: a low-cost open-model subscription the user uses.
# ---------------------------------------------------------------------------
LLM_PRESETS: list[Provider] = [
    Provider("OpenCode Go", "https://opencode.ai/zen/go/v1", "OPENCODE_API_KEY", "deepseek-v4-flash"),
    Provider("OpenCode Zen", "https://opencode.ai/zen/v1", "OPENCODE_API_KEY", "gemini-3.5-flash"),
    Provider("OpenRouter", "https://openrouter.ai/api/v1", "OPENROUTER_API_KEY", ""),
    Provider("Gemini", "https://generativelanguage.googleapis.com/v1beta/openai/",
             "GEMINI_API_KEY", "gemini-2.5-flash",
             models_path="https://generativelanguage.googleapis.com/v1beta/models", auth="query"),
    Provider("DeepSeek", "https://api.deepseek.com/v1", "DEEPSEEK_API_KEY", "deepseek-v4-flash"),
    Provider("OpenAI", "https://api.openai.com/v1", "OPENAI_API_KEY", "gpt-4o-mini"),
    Provider("SiliconFlow", "https://api.siliconflow.cn/v1", "SILICONFLOW_API_KEY", "deepseek-ai/DeepSeek-V3"),
    Provider("GLM (Zhipu)", "https://open.bigmodel.cn/api/paas/v4", "ZHIPU_API_KEY", "glm-4.5-flash"),
    Provider("Kimi (Moonshot)", "https://api.moonshot.cn/v1", "MOONSHOT_API_KEY", "kimi-k2-turbo"),
    Provider("Doubao (Volcano Ark)", "https://ark.cn-beijing.volces.com/api/v3", "ARK_API_KEY", "doubao-lite"),
    Provider("Qwen (Alibaba DashScope)", "https://dashscope.aliyuncs.com/compatible-mode/v1", "DASHSCOPE_API_KEY", "qwen-turbo"),
    Provider("Claude (Anthropic OpenAI layer)", "https://api.anthropic.com/v1", "ANTHROPIC_API_KEY", "claude-haiku-4-5"),
    Provider("Ollama (local)", "http://localhost:11434/v1", None, "llama3.1"),
    Provider("Custom", "", "CUSTOM", ""),
]

# ---------------------------------------------------------------------------
# Embedding presets.
# ---------------------------------------------------------------------------
EMB_PRESETS: list[Provider] = [
    Provider("SiliconFlow", "https://api.siliconflow.cn/v1", "SILICONFLOW_API_KEY", "BAAI/bge-m3", dims=1024),
    Provider("OpenAI embedding", "https://api.openai.com/v1", "OPENAI_API_KEY", "text-embedding-3-small", dims=1536),
    Provider("Gemini embedding", "https://generativelanguage.googleapis.com/v1beta/openai/",
             "GEMINI_API_KEY", "text-embedding-004", dims=3072,
             models_path="https://generativelanguage.googleapis.com/v1beta/models", auth="query"),
    Provider("Ollama (local)", "http://localhost:11434/v1", None, "nomic-embed-text", dims=1024),
    Provider("Custom", "", "CUSTOM", "", dims=1024),
]

# ---------------------------------------------------------------------------
# Live model discovery.
#
# Mirrors Hermes' _CREDENTIAL_PROBES (hermes_cli/web_server.py): a per-key-env
# map of (models_url, auth) so known providers use their correct endpoint and
# auth style. Providers not listed fall back to `{base}{models_path}` with the
# provider's own auth field. httpx's default User-Agent passes Cloudflare
# (urllib's Python-urllib fingerprint is blocked with 403), so requests are
# made with httpx when available and fall back to urllib + a curl UA otherwise.
# ---------------------------------------------------------------------------
_MODEL_PROBES: dict[str, tuple[str, str]] = {
    "OPENAI_API_KEY": ("https://api.openai.com/v1/models", "bearer"),
    "GEMINI_API_KEY": ("https://generativelanguage.googleapis.com/v1beta/models", "query"),
    "OPENROUTER_API_KEY": ("https://openrouter.ai/api/v1/models", "bearer"),
}


def _parse_model_ids(data) -> list[str]:
    """Tolerantly extract model ids from an OpenAI-compatible /models payload.

    Handles both ``{"data": [{"id": ...}, ...]}`` (OpenAI / vLLM / llama.cpp)
    and a bare ``{"data": ["id", ...]}`` shape. Returns [] on any oddity so a
    non-standard endpoint never hard-blocks the installer.
    """
    try:
        payload = data
        items = payload.get("data") if isinstance(payload, dict) else payload
        if not isinstance(items, list):
            return []
        ids: list[str] = []
        for item in items:
            if isinstance(item, dict):
                mid = str(item.get("id") or "").strip()
            else:
                mid = str(item or "").strip()
            if mid:
                ids.append(mid)
        return ids
    except Exception:
        return []


def _models_url(prov: Provider) -> str:
    """The provider's absolute models endpoint URL."""
    if prov.models_path.startswith("http"):
        return prov.models_path
    if prov.url:
        return prov.url.rstrip("/") + prov.models_path
    probe = _MODEL_PROBES.get(prov.key_env or "", (None, None))[0]
    return probe or ""


def _fetch_httpx(prov: Provider) -> list[str]:
    """Fetch model ids via httpx (default UA passes Cloudflare)."""
    import httpx

    url = _models_url(prov)
    if not url:
        return []
    headers = {"Accept": "application/json"}
    params = {}
    if prov.key_env and prov.key_env != "CUSTOM":
        key = os.environ.get(prov.key_env, "")
        if key:
            if prov.auth == "query":
                params["key"] = key
            else:
                headers["Authorization"] = f"Bearer {key}"
    try:
        with httpx.Client(timeout=httpx.Timeout(8.0)) as client:
            resp = client.get(url, headers=headers, params=params)
        if resp.status_code >= 400:
            return []
        return _parse_model_ids(resp.json())
    except Exception:
        return []


def _fetch_urllib(prov: Provider) -> list[str]:
    """Fallback using urllib with a curl User-Agent (dodges Cloudflare 403)."""
    import json
    import urllib.request

    url = _models_url(prov)
    if not url:
        return []
    url_parsed = url
    sep = "&" if "?" in url else "?"
    if prov.key_env and prov.key_env != "CUSTOM":
        key = os.environ.get(prov.key_env, "")
        if key and prov.auth == "query":
            url_parsed = f"{url}{sep}key={key}"
    req = urllib.request.Request(url_parsed, method="GET")
    req.add_header("User-Agent", "curl/8.5.0")
    req.add_header("Accept", "application/json")
    if prov.key_env and prov.key_env != "CUSTOM":
        key = os.environ.get(prov.key_env, "")
        if key and prov.auth != "query":
            req.add_header("Authorization", f"Bearer {key}")
    try:
        with urllib.request.urlopen(req, timeout=8) as resp:
            data = json.loads(resp.read().decode("utf-8"))
    except Exception:
        return []
    return _parse_model_ids(data)


def fetch_models(prov: Provider) -> list[str]:
    """Return the provider's model ids, or [] on any failure.

    Prefers httpx (its User-Agent passes Cloudflare, unlike urllib's default);
    falls back to urllib + curl UA. The API key is attached when set and the
    provider isn't keyless/custom. Any network/auth/parse error silently
    yields [] so the caller can fall back to the flash-tier default.
    """
    if not prov.url and not _MODEL_PROBES.get(prov.key_env or ""):
        return []
    try:
        return _fetch_httpx(prov)
    except ImportError:
        return _fetch_urllib(prov)


def env_present(provider: Provider) -> bool:
    """True when the provider's API key env var is set (or keyless)."""
    if provider.key_env in (None, "CUSTOM"):
        return True
    return bool(os.environ.get(provider.key_env, ""))
