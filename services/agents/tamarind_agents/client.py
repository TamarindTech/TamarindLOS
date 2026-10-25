"""Client for the Tamarind tool gateway.

Agents call tools over MCP (Streamable HTTP, JSON responses) exactly like an
external agent would, and follow the tenant's event feed over REST. Only the
standard library is used, so the runtime has no third-party dependencies.
"""

from __future__ import annotations

import itertools
import json
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass, field
from typing import Any


class GatewayError(Exception):
    """The gateway could not be reached or answered with a protocol error."""


@dataclass
class ToolResult:
    """The gateway's response to one tool call (Spec 03 §5)."""

    status: str  # ok | approval_required | error
    result: dict[str, Any] = field(default_factory=dict)
    approval_request_id: str = ""
    error_code: str = ""
    error_message: str = ""
    replayed: bool = False

    @property
    def ok(self) -> bool:
        return self.status == "ok"

    @classmethod
    def from_response(cls, data: dict[str, Any]) -> "ToolResult":
        err = data.get("error") or {}
        return cls(
            status=data.get("status", "error"),
            result=data.get("result") or {},
            approval_request_id=data.get("approval_request_id", ""),
            error_code=err.get("code", ""),
            error_message=err.get("message", ""),
            replayed=bool(data.get("replayed")),
        )


class TamarindClient:
    """Calls tools through MCP and reads the event feed with one bearer token."""

    def __init__(self, base_url: str, token: str, timeout: float = 40.0) -> None:
        self.base_url = base_url.rstrip("/")
        self.token = token
        self.timeout = timeout
        self._ids = itertools.count(1)
        self._initialized = False
        # Set by the runtime while an agent run is open: every call is linked to it.
        self.run_id: str | None = None

    # --- transport -----------------------------------------------------------

    def _request(self, method: str, path: str, body: Any = None, timeout: float | None = None) -> Any:
        data = None if body is None else json.dumps(body).encode()
        req = urllib.request.Request(self.base_url + path, data=data, method=method)
        req.add_header("Authorization", f"Bearer {self.token}")
        if data is not None:
            req.add_header("Content-Type", "application/json")
        try:
            with urllib.request.urlopen(req, timeout=timeout or self.timeout) as res:
                return json.loads(res.read() or b"null")
        except urllib.error.HTTPError as e:
            payload = e.read()
            try:
                return json.loads(payload)
            except ValueError:
                raise GatewayError(f"{method} {path}: HTTP {e.code}") from e
        except (urllib.error.URLError, TimeoutError, ConnectionError) as e:
            raise GatewayError(f"{method} {path}: {e}") from e

    def _rpc(self, method: str, params: dict[str, Any]) -> dict[str, Any]:
        msg = {"jsonrpc": "2.0", "id": next(self._ids), "method": method, "params": params}
        out = self._request("POST", "/mcp", msg)
        if not isinstance(out, dict):
            raise GatewayError(f"MCP {method}: unexpected response")
        if out.get("error"):
            raise GatewayError(f"MCP {method}: {out['error'].get('message')}")
        return out.get("result") or {}

    # --- MCP -------------------------------------------------------------------

    def initialize(self) -> dict[str, Any]:
        info = self._rpc("initialize", {"protocolVersion": "2025-06-18", "capabilities": {},
                                        "clientInfo": {"name": "tamarind-agents", "version": "0.1.0"}})
        self._initialized = True
        return info

    def list_tools(self) -> list[str]:
        if not self._initialized:
            self.initialize()
        return [t["name"] for t in self._rpc("tools/list", {}).get("tools", [])]

    def call(self, tool: str, args: dict[str, Any], *, reason: str, key: str | None = None,
             on_behalf_of: str | None = None) -> ToolResult:
        """Invoke a catalogue tool (dotted name) with the call envelope."""
        if not self._initialized:
            self.initialize()
        envelope: dict[str, Any] = {"reason": reason}
        if key:
            envelope["idempotency_key"] = key
        if on_behalf_of:
            envelope["on_behalf_of"] = on_behalf_of
        if self.run_id:
            envelope["run_id"] = self.run_id
        result = self._rpc("tools/call", {"name": tool.replace(".", "_"), "arguments": {**args, "_envelope": envelope}})
        structured = result.get("structuredContent")
        if not isinstance(structured, dict):
            raise GatewayError(f"{tool}: response has no structured content")
        return ToolResult.from_response(structured)

    # --- event feed ----------------------------------------------------------

    def events(self, after: int, *, types: list[str] | None = None, wait: int = 0, limit: int = 100) -> dict[str, Any]:
        """Read the tenant's events after a sequence number, long-polling up to `wait` seconds."""
        query = {"after": after, "limit": limit, "wait": wait}
        if types:
            query["types"] = ",".join(types)
        out = self._request("GET", "/v1/events?" + urllib.parse.urlencode(query), timeout=wait + self.timeout)
        if not isinstance(out, dict) or "events" not in out:
            err = (out or {}).get("error", {}) if isinstance(out, dict) else {}
            raise GatewayError(f"event feed: {err.get('code', 'bad response')} {err.get('message', '')}".strip())
        return out
