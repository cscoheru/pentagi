"""OpenAI 兼容 fallback proxy: MiniMax primary → lilith tailnet LLM fallback.

Designed for PentAGI LLM_SERVER_URL integration. Pass-through HTTP/SSE with
one decision point: when primary returns a quota/rate-limit status code, retry
the same request against lilith's ollama. Otherwise forward primary's response
verbatim — including tool-calling, response_format, thinking blocks, etc.
"""

import json
import logging
import os

import httpx
from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse, Response, StreamingResponse

LOG = logging.getLogger("llm-fallback")
logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")

PRIMARY_URL = os.environ["PRIMARY_URL"]                       # MiniMax upstream
PRIMARY_KEY = os.environ.get("PRIMARY_KEY", "")               # MiniMax Bearer
PRIMARY_MODEL = os.environ.get("PRIMARY_MODEL", "")           # override if upstream passes empty model

FALLBACK_URL = os.environ["FALLBACK_URL"]                     # lilith ollama /v1/chat/completions
FALLBACK_MODEL = os.environ.get("FALLBACK_MODEL", "qwen2.5:7b")

PRIMARY_TIMEOUT = float(os.environ.get("PRIMARY_TIMEOUT", "8"))
FALLBACK_TIMEOUT = float(os.environ.get("FALLBACK_TIMEOUT", "30"))

# MiniMax 配额/限流类错误码 — 触发 fallback（避免用错 fallback 而把瞬时错误当成配额问题）
QUOTA_STATUS = {402, 429, 2056}

app = FastAPI()


def _build_primary_body(body_bytes: bytes) -> bytes:
    """如果客户端没传 model 或传空，则用 PRIMARY_MODEL。"""
    if not PRIMARY_MODEL:
        return body_bytes
    try:
        payload = json.loads(body_bytes)
    except json.JSONDecodeError:
        return body_bytes
    if not payload.get("model"):
        payload["model"] = PRIMARY_MODEL
        return json.dumps(payload).encode()
    return body_bytes


def _build_fallback_body(body_bytes: bytes) -> bytes:
    """强制 fallback 走 lilith 模型名 + 去掉 Ollama 不识别的 thinking 字段。"""
    try:
        payload = json.loads(body_bytes)
    except json.JSONDecodeError:
        return body_bytes
    payload["model"] = FALLBACK_MODEL
    payload.pop("thinking", None)
    return json.dumps(payload).encode()


async def _primary_response(body_bytes: bytes):
    """Try primary (MiniMax). Returns (status_code, content, content_type) or None on transport error."""
    headers = {"Content-Type": "application/json"}
    if PRIMARY_KEY:
        headers["Authorization"] = f"Bearer {PRIMARY_KEY}"

    body = _build_primary_body(body_bytes)
    async with httpx.AsyncClient(timeout=PRIMARY_TIMEOUT) as c:
        try:
            r = await c.post(PRIMARY_URL, headers=headers, content=body)
        except httpx.TimeoutException:
            LOG.warning("primary timeout (%.1fs)", PRIMARY_TIMEOUT)
            return None
        except httpx.RequestError as e:
            LOG.warning("primary transport error: %s", e)
            return None
    return r.status_code, r.content, r.headers.get("content-type", "application/json")


async def _fallback_stream(body_bytes: bytes):
    """Stream fallback (lilith) via SSE pass-through."""
    headers = {"Content-Type": "application/json"}
    body = _build_fallback_body(body_bytes)
    timeout = httpx.Timeout(FALLBACK_TIMEOUT, read=FALLBACK_TIMEOUT)

    async def gen():
        async with httpx.AsyncClient(timeout=timeout) as c:
            async with c.stream("POST", FALLBACK_URL, headers=headers, content=body) as r:
                async for chunk in r.aiter_bytes():
                    yield chunk

    return gen()


async def _fallback_response(body_bytes: bytes):
    """Non-stream fallback (lilith). Returns (status_code, content, content_type)."""
    headers = {"Content-Type": "application/json"}
    body = _build_fallback_body(body_bytes)
    async with httpx.AsyncClient(timeout=FALLBACK_TIMEOUT) as c:
        try:
            r = await c.post(FALLBACK_URL, headers=headers, content=body)
        except (httpx.TimeoutException, httpx.RequestError) as e:
            LOG.error("fallback failed: %s", e)
            return None
    return r.status_code, r.content, r.headers.get("content-type", "application/json")


@app.post("/v1/chat/completions")
async def chat_completions(request: Request):
    body_bytes = await request.body()
    is_stream = b'"stream":true' in body_bytes or b'"stream": true' in body_bytes

    primary = await _primary_response(body_bytes)
    if primary is not None:
        status, content, ctype = primary
        if status not in QUOTA_STATUS:
            return Response(content=content, status_code=status, headers={"Content-Type": ctype})
        LOG.warning("primary status %d (quota/rate-limit) → fallback to lilith", status)

    # —— Fallback to lilith ——
    if is_stream:
        return StreamingResponse(_fallback_stream(body_bytes),
                                 media_type="text/event-stream",
                                 headers={"Cache-Control": "no-cache", "X-Accel-Buffering": "no"})

    fb = await _fallback_response(body_bytes)
    if fb is None:
        return JSONResponse(
            {"error": "primary quota-exceeded and fallback unreachable"},
            status_code=502,
        )
    status, content, ctype = fb
    return Response(content=content, status_code=status, headers={"Content-Type": ctype})


@app.get("/healthz")
async def healthz():
    return {"ok": True, "primary": PRIMARY_URL, "fallback": FALLBACK_URL}