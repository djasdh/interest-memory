"""interest-memory bridge — Hermes MemoryProvider interface.

Bridges Hermes conversations to the local interest-memory service via REST:

  * ``prefetch(query)``  → GET  /api/v1/{agent}/recall  (session-start recall)
  * ``on_session_end()`` → POST /api/v1/{agent}/sessions (session-end ingest)

The memory backend does the heavy lifting (fork/verify/interest/wiki/recall).
This provider is a thin HTTP adapter: it buffers turns in memory
during a session and pushes the full transcript once at session end.

Config via environment variables:
  INTEREST_BASE_URL  — interest-memory service base URL (default: http://127.0.0.1:8899)
  INTEREST_TIMEOUT   — per-request timeout seconds (default: 8)
  INTEREST_AGENT     — override agent namespace (default: Hermes profile via agent_identity)

Deploy by placing this directory as $HERMES_HOME/plugins/interest/ and setting
``memory.provider: interest`` in config.yaml. Hermes discovers MemoryProvider
subclasses automatically (see plugins/memory/__init__.py).
"""

from __future__ import annotations

import datetime
import json
import logging
import os
import threading
import time as _time
from typing import Any, Dict, List, Optional

from agent.memory_provider import MemoryProvider

logger = logging.getLogger(__name__)

_DEFAULT_BASE_URL = "http://127.0.0.1:8899"
_DEFAULT_TIMEOUT = 8.0
_SESSION_END_TIMEOUT = 15.0


class InterestMemoryProvider(MemoryProvider):
    """Hermes ↔ interest-memory REST bridge."""

    def __init__(self) -> None:
        self._base_url = _DEFAULT_BASE_URL
        self._timeout = _DEFAULT_TIMEOUT
        self._agent_id = "default"
        self._session_id = ""
        self._turn_count = 0
        self._turns: List[Dict[str, Any]] = []
        self._lock = threading.Lock()
        self._skip_writes = False
        self._session_started_at = 0.0

    # -- Core lifecycle -----------------------------------------------------

    @property
    def name(self) -> str:
        return "interest"

    def is_available(self) -> bool:
        """Availability is env-only — no network calls (MemoryProvider contract)."""
        return bool(os.environ.get("INTEREST_BASE_URL"))

    def initialize(self, session_id: str, **kwargs) -> None:
        base = os.environ.get("INTEREST_BASE_URL", _DEFAULT_BASE_URL).rstrip("/")
        self._base_url = base
        try:
            self._timeout = float(os.environ.get("INTEREST_TIMEOUT", _DEFAULT_TIMEOUT))
        except (TypeError, ValueError):
            self._timeout = _DEFAULT_TIMEOUT

        # agent namespace: explicit env > Hermes profile (agent_identity).
        agent = os.environ.get("INTEREST_AGENT", "")
        if not agent:
            agent = kwargs.get("agent_identity") or kwargs.get("profile") or "default"
        self._agent_id = str(agent)

        # Non-primary contexts (cron system prompts, subagent flushes) would
        # corrupt the user representation — skip writes there.
        if kwargs.get("agent_context", "primary") != "primary":
            self._skip_writes = True

        self._session_id = session_id
        self._session_started_at = _time.time()
        with self._lock:
            self._turns = []
            self._turn_count = 0

    def on_session_switch(self, new_session_id: str, **kwargs) -> None:
        """Reset the session-start timestamp when Hermes rotates session ids
        (/new, /resume, compression continuation) so session_date reflects the
        actual start of the new logical session."""
        self._session_id = new_session_id
        self._session_started_at = _time.time()
        with self._lock:
            self._turns = []
            self._turn_count = 0

    def _session_date_rfc3339(self) -> str:
        """RFC3339 UTC string of the session start time (fallback: now)."""
        ts = self._session_started_at or _time.time()
        return datetime.datetime.fromtimestamp(ts, tz=datetime.timezone.utc).isoformat()

    def system_prompt_block(self) -> str:
        return ""

    def prefetch(self, query: str, *, session_id: str = "") -> str:
        """Recall memory context before the upcoming turn.

        Returns bare text; Hermes wraps it in <memory-context> via
        build_memory_context_block. On any failure returns "" so the turn is
        never blocked (failure isolation).
        """
        if self._skip_writes or not query:
            return ""
        try:
            import requests
            url = f"{self._base_url}/api/v1/{self._agent_id}/recall"
            resp = requests.get(url, params={"query": query}, timeout=self._timeout)
            if resp.status_code != 200:
                logger.debug("interest recall %s → %d", url, resp.status_code)
                return ""
            payload = resp.json()
            return str(payload.get("memory_context") or "")
        except Exception as exc:
            logger.debug("interest recall failed: %s", exc)
            return ""

    # -- Turn buffering ------------------------------------------------------

    def sync_turn(
        self,
        user_content: str,
        assistant_content: str,
        *,
        session_id: str = "",
        messages: Optional[List[Dict[str, Any]]] = None,
    ) -> None:
        """Buffer a completed turn in memory. Actual ingest happens at session
        end (on_session_end) — the backend worker serializes per agent and
        keeps failed transcripts for retry, so no local queue is needed.
        """
        if self._skip_writes:
            return
        if messages:
            rows = _extract_turns(messages)
            if rows:
                with self._lock:
                    self._turns.extend(rows)
                    self._turn_count += len(rows)
                return
        with self._lock:
            if user_content:
                self._turns.append({"role": "user", "content": user_content})
                self._turn_count += 1
            if assistant_content:
                self._turns.append({"role": "assistant", "content": assistant_content})
                self._turn_count += 1

    def on_session_end(self, messages: List[Dict[str, Any]]) -> None:
        """Push the full buffered transcript to the backend."""
        if self._skip_writes:
            return
        # If the manager passes the full history, prefer it over the buffer.
        rows = _extract_turns(messages)
        with self._lock:
            if rows and len(rows) >= self._turn_count:
                turns = rows
            else:
                turns = list(self._turns)
            self._turns = []
            self._turn_count = 0
        if not turns:
            return
        try:
            import requests
            url = f"{self._base_url}/api/v1/{self._agent_id}/sessions"
            resp = requests.post(
                url,
                json={
                    "session_id": self._session_id,
                    "turn_count": len(turns),
                    "raw_turns": json.dumps(turns, ensure_ascii=False),
                    "session_date": self._session_date_rfc3339(),
                },
                timeout=_SESSION_END_TIMEOUT,
            )
            if resp.status_code not in (200, 201, 202):
                logger.warning("interest ingest %s → %d: %s", url, resp.status_code, resp.text[:200])
        except Exception as exc:
            logger.warning("interest ingest failed (will be lost, backend keeps transcripts): %s", exc)

    def shutdown(self) -> None:
        """Best-effort flush of any buffered turns not delivered by a normal
        session end."""
        try:
            self.on_session_end([])
        except Exception:
            pass

    # -- Tools ---------------------------------------------------------------

    def get_tool_schemas(self) -> List[Dict[str, Any]]:
        """Expose the consumer-side memory_search tool.

        - ``query``: semantic retrieval over interest points + wiki pages.
          Returns full content (body/claims/evidence) plus adjacency edges
          (outlinks/backlinks with far-end titles).
        - ``id``: fetch one specific page or interest point by id (full
          content + edges). Takes precedence over query when both given.
        - ``top_k``: max hits for query search (default 3).
        """
        return [
            {
                "name": "memory_search",
                "description": (
                    "Search the interest-memory knowledge base and return full "
                    "entries (body/claims/evidence) with their relationship edges. "
                    "Pass 'query' for a semantic search, or 'id' to fetch one "
                    "specific page/interest point by id (id wins when both are given)."
                ),
                "parameters": {
                    "type": "object",
                    "properties": {
                        "query": {
                            "type": "string",
                            "description": "Semantic search query (e.g. a topic, decision, or phrase)",
                        },
                        "id": {
                            "type": "string",
                            "description": "Exact id of a wiki page or interest point to fetch",
                        },
                        "top_k": {
                            "type": "number",
                            "description": "Max results for query search (default 3)",
                        },
                    },
                },
            },
            {
                "name": "memory_logs",
                "description": (
                    "Query the change-log of the interest-memory knowledge base: "
                    "recent structural changes (page/interest-point title, action, "
                    "and edges touched), newest first."
                ),
                "parameters": {
                    "type": "object",
                    "properties": {
                        "limit": {
                            "type": "number",
                            "description": "Max log entries to return (default 10)",
                        },
                        "offset": {
                            "type": "number",
                            "description": "Pagination offset (default 0)",
                        },
                    },
                },
            },
        ]

    def handle_tool_call(self, tool_name: str, args: Dict[str, Any], **kwargs) -> str:
        """Dispatch a memory provider tool call. Returns a JSON string."""
        if tool_name == "memory_logs":
            return self._memory_logs(args)
        if tool_name != "memory_search":
            raise NotImplementedError(f"Provider {self.name} does not handle tool {tool_name}")
        query = str(args.get("query") or "")
        entity_id = str(args.get("id") or "")
        try:
            top_k = int(args.get("top_k") or 0)
        except (TypeError, ValueError):
            top_k = 0
        if not query and not entity_id:
            return json.dumps({"error": "memory_search: missing 'query' or 'id'"}, ensure_ascii=False)
        try:
            import requests
            url = f"{self._base_url}/api/v1/{self._agent_id}/search"
            params: Dict[str, Any] = {}
            if entity_id:
                params["id"] = entity_id
            else:
                params["query"] = query
                if top_k > 0:
                    params["top_k"] = top_k
            resp = requests.get(url, params=params, timeout=self._timeout)
            if resp.status_code != 200:
                return json.dumps({"error": f"memory_search: status {resp.status_code}"}, ensure_ascii=False)
            return json.dumps(resp.json().get("items", []), ensure_ascii=False)
        except Exception as exc:
            return json.dumps({"error": f"memory_search: {exc}"}, ensure_ascii=False)

    def _memory_logs(self, args: Dict[str, Any]) -> str:
        """memory_logs: query recent change logs (newest first)."""
        try:
            limit = int(args.get("limit") or 10)
            offset = int(args.get("offset") or 0)
        except (TypeError, ValueError):
            limit, offset = 10, 0
        if limit < 0:
            limit = 10
        if offset < 0:
            offset = 0
        try:
            import requests
            url = f"{self._base_url}/api/v1/{self._agent_id}/logs"
            resp = requests.get(url, params={"limit": limit, "offset": offset}, timeout=self._timeout)
            if resp.status_code != 200:
                return json.dumps({"error": f"memory_logs: status {resp.status_code}"}, ensure_ascii=False)
            return json.dumps(resp.json().get("items", []), ensure_ascii=False)
        except Exception as exc:
            return json.dumps({"error": f"memory_logs: {exc}"}, ensure_ascii=False)


def _extract_turns(messages: List[Dict[str, Any]]) -> List[Dict[str, str]]:
    """Reduce OpenAI-style messages (may contain tool_calls/tool_call_id) to
    [{role, content}] — the wire format the backend's transcript parser reads.
    """
    out: List[Dict[str, str]] = []
    for m in messages or []:
        role = str(m.get("role") or "")
        content = m.get("content")
        if content is None:
            continue
        text = content if isinstance(content, str) else json.dumps(content, ensure_ascii=False)
        text = text.strip()
        if not text:
            continue
        if role in {"user", "assistant", "tool", "tool_result"}:
            out.append({"role": role, "content": text})
        elif role == "system":
            continue
    return out


def register(ctx) -> None:
    """Register the provider with the Hermes plugin context."""
    ctx.register_memory_provider(InterestMemoryProvider())
