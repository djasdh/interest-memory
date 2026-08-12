#!/usr/bin/env python3
"""Standalone unit tests for the interest-memory Hermes bridge.

Runs without a Hermes runtime: stubs out ``agent.memory_provider`` and
``requests`` so the provider's pure logic (turn buffering, extraction, HTTP
shaping) can be exercised.

Usage: python3 bridge/hermes/test_interest.py
"""

import importlib
import json
import os
import sys
import time
import datetime
import types
import unittest
from unittest import mock


def _load_provider():
    """Import __init__.py with stubbed agent.memory_provider."""
    # Stub the hermes runtime module the plugin imports.
    mem_provider = types.ModuleType("agent.memory_provider")

    class MemoryProvider:
        name = "base"
        is_available = lambda self: True  # noqa: E731
        initialize = lambda self, *a, **k: None  # noqa: E731
        system_prompt_block = lambda self: ""  # noqa: E731
        prefetch = lambda self, *a, **k: ""  # noqa: E731
        sync_turn = lambda self, *a, **k: None  # noqa: E731
        get_tool_schemas = lambda self: []  # noqa: E731
        handle_tool_call = lambda self, *a, **k: "{}"  # noqa: E731
        shutdown = lambda self: None  # noqa: E731
        on_session_end = lambda self, *a, **k: None  # noqa: E731

    mem_provider.MemoryProvider = MemoryProvider

    agent_pkg = types.ModuleType("agent")
    agent_pkg.__path__ = []
    sys.modules["agent"] = agent_pkg
    sys.modules["agent.memory_provider"] = mem_provider

    path = os.path.join(os.path.dirname(__file__), "__init__.py")
    spec = importlib.util.spec_from_file_location("_interest_bridge_test", path)
    mod = importlib.util.module_from_spec(spec)
    sys.modules["_interest_bridge_test"] = mod
    spec.loader.exec_module(mod)
    return mod


MOD = _load_provider()


class ExtractTurnsTest(unittest.TestCase):
    def test_reduces_openai_messages(self):
        msgs = [
            {"role": "user", "content": "hello"},
            {"role": "assistant", "content": "hi", "tool_calls": []},
            {"role": "tool", "content": "result"},
            {"role": "system", "content": "ignored"},
            {"role": "user", "content": ""},
            {"role": "user", "content": None},
        ]
        out = MOD._extract_turns(msgs)
        self.assertEqual(
            out,
            [
                {"role": "user", "content": "hello"},
                {"role": "assistant", "content": "hi"},
                {"role": "tool", "content": "result"},
            ],
        )

    def test_structured_content_stringified(self):
        msgs = [{"role": "assistant", "content": [{"type": "text", "text": "x"}]}]
        out = MOD._extract_turns(msgs)
        self.assertEqual(len(out), 1)
        self.assertEqual(out[0]["role"], "assistant")
        parsed = json.loads(out[0]["content"])
        self.assertEqual(parsed[0]["text"], "x")


class ProviderTest(unittest.TestCase):
    def setUp(self):
        os.environ.pop("INTEREST_BASE_URL", None)
        os.environ.pop("INTEREST_AGENT", None)
        os.environ.pop("INTEREST_TIMEOUT", None)
        os.environ.pop("INTEREST_MODE", None)
        import tempfile
        self._state_dir = tempfile.mkdtemp(prefix="interest-state-")
        os.environ["INTEREST_STATE_FILE"] = os.path.join(self._state_dir, "state.json")
        self.p = MOD.InterestMemoryProvider()

    def tearDown(self):
        os.environ.pop("INTEREST_STATE_FILE", None)
        import shutil
        shutil.rmtree(self._state_dir, ignore_errors=True)

    def test_name_and_availability(self):
        self.assertEqual(self.p.name, "interest")
        self.assertFalse(self.p.is_available())  # no env
        os.environ["INTEREST_BASE_URL"] = "http://x:1"
        self.assertTrue(self.p.is_available())

    def test_initialize_sets_agent_from_identity(self):
        self.p.initialize("s1", agent_identity="coder")
        self.assertEqual(self.p._agent_id, "coder")
        self.assertEqual(self.p._session_id, "s1")

    def test_initialize_skips_writes_for_non_primary(self):
        self.p.initialize("s1", agent_context="cron")
        self.assertTrue(self.p._skip_writes)

    def test_sync_turn_buffers(self):
        self.p.initialize("s1")
        self.p.sync_turn("u1", "a1")
        self.p.sync_turn("u2", "a2")
        with self.p._lock:
            self.assertEqual(len(self.p._turns), 4)
            self.assertEqual(self.p._turn_count, 4)

    def test_sync_turn_uses_messages_when_provided(self):
        self.p.initialize("s1")
        self.p.sync_turn("ignored", "ignored", messages=[
            {"role": "user", "content": "m1"},
            {"role": "assistant", "content": "m2"},
        ])
        with self.p._lock:
            self.assertEqual(self.p._turns[0]["content"], "m1")
            self.assertEqual(self.p._turn_count, 2)

    def test_on_session_end_posts_transcript(self):
        self.p.initialize("s1")
        self.p.sync_turn("u1", "a1")
        self.p.sync_turn("u2", "a2")
        with mock.patch("requests.post") as post:
            resp = mock.MagicMock()
            resp.status_code = 202
            post.return_value = resp
            self.p.on_session_end([])
        post.assert_called_once()
        url, kwargs = post.call_args
        self.assertIn("/api/v1/default/sessions", url[0])
        body = kwargs["json"]
        self.assertEqual(body["session_id"], "s1")
        self.assertEqual(body["turn_count"], 4)
        raw = json.loads(body["raw_turns"])
        self.assertEqual(len(raw), 4)
        self.assertEqual(raw[0], {"role": "user", "content": "u1"})

    def test_on_session_end_prefers_full_history(self):
        self.p.initialize("s1")
        self.p.sync_turn("buffered", "x")
        history = [{"role": "user", "content": "full1"}, {"role": "assistant", "content": "full2"}]
        with mock.patch("requests.post") as post:
            resp = mock.MagicMock()
            resp.status_code = 202
            post.return_value = resp
            self.p.on_session_end(history)
        body = post.call_args.kwargs["json"]
        raw = json.loads(body["raw_turns"])
        self.assertEqual(raw, history)

    def test_prefetch_returns_bare_text(self):
        self.p.initialize("s1")
        with mock.patch("requests.get") as get:
            resp = mock.MagicMock()
            resp.status_code = 200
            resp.json.return_value = {"memory_context": "- Go [interest_point]"}
            get.return_value = resp
            out = self.p.prefetch("golang")
        self.assertEqual(out, "- Go [interest_point]")

    def test_prefetch_failure_is_isolated(self):
        self.p.initialize("s1")
        with mock.patch("requests.get", side_effect=RuntimeError("boom")):
            self.assertEqual(self.p.prefetch("q"), "")

    def test_shutdown_flushes_buffer(self):
        self.p.initialize("s1")
        self.p.sync_turn("u1", "a1")
        with mock.patch("requests.post") as post:
            resp = mock.MagicMock()
            resp.status_code = 202
            post.return_value = resp
            self.p.shutdown()
        self.assertTrue(post.called)

    # -- session_date passthrough -------------------------------------------

    def test_initialize_records_session_start(self):
        self.p.initialize("s1")
        self.assertGreater(self.p._session_started_at, 0)

    def test_on_session_switch_resets_session_start(self):
        self.p.initialize("s1")
        t0 = self.p._session_started_at
        time.sleep(0.01)
        self.p.on_session_switch("s2")
        self.assertGreater(self.p._session_started_at, t0)

    def test_on_session_end_posts_session_date(self):
        self.p.initialize("s1")
        self.p.sync_turn("u1", "a1")
        with mock.patch("requests.post") as post:
            resp = mock.MagicMock()
            resp.status_code = 202
            post.return_value = resp
            self.p.on_session_end([])
        payload = post.call_args[1]["json"]
        sd = payload.get("session_date")
        self.assertTrue(sd, "session_date missing from payload")
        dt = datetime.datetime.fromisoformat(sd.replace("Z", "+00:00"))
        self.assertIsNotNone(dt.tzinfo)

    # -- memory_search tool --------------------------------------------------

    def test_get_tool_schemas_exposes_memory_search(self):
        schemas = self.p.get_tool_schemas()
        names = {s["name"] for s in schemas}
        self.assertIn("memory_search", names)
        self.assertIn("memory_logs", names)
        s = next(s for s in schemas if s["name"] == "memory_search")
        self.assertIn("parameters", s)
        self.assertIn("query", s["parameters"]["properties"])
        self.assertIn("id", s["parameters"]["properties"])
        self.assertIn("top_k", s["parameters"]["properties"])

    def test_handle_tool_call_search_by_query(self):
        self.p.initialize("s1")
        with mock.patch("requests.get") as get:
            resp = mock.MagicMock()
            resp.status_code = 200
            resp.json.return_value = {
                "items": [{"kind": "wiki_page", "id": "pg", "title": "T",
                           "outlinks": [{"id": "x", "title": "X", "kind": "related"}]}]
            }
            get.return_value = resp
            out = self.p.handle_tool_call("memory_search", {"query": "postgresql", "top_k": 5})
        get.assert_called_once()
        url = get.call_args[0][0]
        self.assertIn("/search", url)
        params = get.call_args[1].get("params", {})
        self.assertEqual(params.get("query"), "postgresql")
        self.assertEqual(params.get("top_k"), 5)
        payload = json.loads(out)
        self.assertEqual(payload[0]["id"], "pg")

    def test_handle_tool_call_search_by_id(self):
        self.p.initialize("s1")
        with mock.patch("requests.get") as get:
            resp = mock.MagicMock()
            resp.status_code = 200
            resp.json.return_value = {"items": [{"kind": "interest_point", "id": "ip-1"}]}
            get.return_value = resp
            out = self.p.handle_tool_call("memory_search", {"id": "ip-1"})
        params = get.call_args[1].get("params", {})
        self.assertEqual(params.get("id"), "ip-1")
        self.assertNotIn("query", params)
        self.assertEqual(json.loads(out)[0]["id"], "ip-1")

    def test_handle_tool_call_missing_args(self):
        self.p.initialize("s1")
        out = self.p.handle_tool_call("memory_search", {})
        self.assertIn("error", json.loads(out))

    def test_handle_tool_call_failure_isolated(self):
        self.p.initialize("s1")
        with mock.patch("requests.get", side_effect=RuntimeError("boom")):
            out = self.p.handle_tool_call("memory_search", {"query": "q"})
        self.assertIn("error", json.loads(out))

    def test_handle_tool_call_unknown_tool(self):
        self.p.initialize("s1")
        with self.assertRaises(NotImplementedError):
            self.p.handle_tool_call("nope", {})

    # -- memory_logs tool ----------------------------------------------------

    def test_handle_tool_call_logs(self):
        self.p.initialize("s1")
        with mock.patch("requests.get") as get:
            resp = mock.MagicMock()
            resp.status_code = 200
            resp.json.return_value = {
                "items": [{"id": "l1", "action": "create", "title": "P1",
                           "edges": [{"action": "add", "kind": "has_page"}]}]
            }
            get.return_value = resp
            out = self.p.handle_tool_call("memory_logs", {"limit": 5})
        params = get.call_args[1].get("params", {})
        self.assertEqual(params.get("limit"), 5)
        self.assertEqual(params.get("offset"), 0)
        items = json.loads(out)
        self.assertEqual(items[0]["id"], "l1")

    def test_handle_tool_call_logs_failure_isolated(self):
        self.p.initialize("s1")
        with mock.patch("requests.get", side_effect=RuntimeError("boom")):
            out = self.p.handle_tool_call("memory_logs", {})
        self.assertIn("error", json.loads(out))


class ModeTest(unittest.TestCase):
    def setUp(self):
        os.environ.pop("INTEREST_MODE", None)
        os.environ.pop("INTEREST_STATE_FILE", None)
        self.p = MOD.InterestMemoryProvider()

    def test_mode_defaults_auto(self):
        self.p.initialize("s1")
        self.assertEqual(self.p._mode, "auto")
        self.assertFalse(self.p._skip_recall)
        self.assertFalse(self.p._skip_ingest)

    def test_input_mode_skips_recall_and_tools(self):
        os.environ["INTEREST_MODE"] = "input"
        self.p.initialize("s1")
        self.assertTrue(self.p._skip_recall)
        self.assertFalse(self.p._skip_ingest)
        self.assertEqual(self.p.prefetch("query"), "")
        self.assertEqual(self.p.get_tool_schemas(), [])

    def test_output_mode_skips_ingest(self):
        os.environ["INTEREST_MODE"] = "output"
        self.p.initialize("s1")
        self.assertFalse(self.p._skip_recall)
        self.assertTrue(self.p._skip_ingest)
        with mock.patch("requests.post") as post:
            self.p.on_session_end([])
            post.assert_not_called()

    def test_bogus_mode_falls_back_auto(self):
        os.environ["INTEREST_MODE"] = "bogus"
        self.p.initialize("s1")
        self.assertEqual(self.p._mode, "auto")


class StateDedupeTest(unittest.TestCase):
    def setUp(self):
        import tempfile
        self.dir = tempfile.mkdtemp(prefix="interest-state-")
        os.environ["INTEREST_STATE_FILE"] = os.path.join(self.dir, "state.json")

    def tearDown(self):
        os.environ.pop("INTEREST_STATE_FILE", None)
        import shutil
        shutil.rmtree(self.dir, ignore_errors=True)

    def test_pushed_key_persists_and_caps(self):
        self.assertEqual(MOD._pushed_key("agent-a", "s1"), "")
        MOD._set_pushed_key("agent-a", "s1", "key-1")
        self.assertEqual(MOD._pushed_key("agent-a", "s1"), "key-1")
        for i in range(2, 12):
            MOD._set_pushed_key("agent-a", f"s{i}", f"key-{i}")
        self.assertEqual(MOD._pushed_key("agent-a", "s1"), "")
        self.assertEqual(MOD._pushed_key("agent-a", "s2"), "key-2")
        self.assertEqual(MOD._pushed_key("agent-a", "s11"), "key-11")


if __name__ == "__main__":
    unittest.main(verbosity=2)
