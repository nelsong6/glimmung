"""In-memory fake of Cosmos `ContainerProxy` for deterministic unit tests.

Models the small surface our code uses: `read_item`, `create_item`,
`replace_item`, `query_items`, with `_etag` semantics + `IfNotModified`
preconditions. Behavior matches Cosmos closely enough that every code
path that touches a container can be covered without a live account:

- `_etag` advances on every successful write; `IfNotModified` raises
  `CosmosAccessConditionFailedError` if the supplied etag is stale.
- `create_item` raises `CosmosResourceExistsError` on duplicate id.
- `read_item` raises `CosmosResourceNotFoundError` on missing id.
- `query_items` is async-iterable like the real client; we evaluate
  the SQL ourselves (a tiny subset — see `_evaluate_query`).

Time is injectable via `now_factory` so TTL/expiry tests are deterministic.
"""

from __future__ import annotations

import re
from datetime import UTC, datetime
from typing import Any, AsyncIterator, Callable

from azure.core import MatchConditions
from azure.cosmos.exceptions import (
    CosmosAccessConditionFailedError,
    CosmosResourceExistsError,
    CosmosResourceNotFoundError,
)


class FakeContainer:
    """One in-memory container. Items keyed by (partition_key, id)."""

    def __init__(self, name: str, partition_key_path: str):
        self.name = name
        # /scope -> "scope"
        self._pk_field = partition_key_path.lstrip("/")
        # (pk_value, id) -> doc
        self._items: dict[tuple[str, str], dict[str, Any]] = {}
        self._etag_counter = 0

    # ─── etag helper ─────────────────────────────────────────────────────────

    def _next_etag(self) -> str:
        self._etag_counter += 1
        return f"etag-{self._etag_counter}"

    def _stored(self, doc: dict[str, Any]) -> dict[str, Any]:
        """Return a defensive copy with `_etag` set. Cosmos always returns
        the etag on successful reads/writes."""
        return {**doc}

    # ─── partition-key resolution ────────────────────────────────────────────

    def _pk_value(self, doc: dict[str, Any]) -> str:
        if self._pk_field not in doc:
            raise ValueError(f"doc missing partition key field {self._pk_field!r}: {doc}")
        return doc[self._pk_field]

    # ─── public API mirror ───────────────────────────────────────────────────

    async def create_item(self, body: dict[str, Any]) -> dict[str, Any]:
        pk = self._pk_value(body)
        doc_id = body["id"]
        if (pk, doc_id) in self._items:
            raise CosmosResourceExistsError(
                message=f"Resource with id {doc_id!r} already exists",
                response=None,
            )
        stored = {**body, "_etag": self._next_etag()}
        self._items[(pk, doc_id)] = stored
        return self._stored(stored)

    async def read_item(self, item: str, partition_key: str) -> dict[str, Any]:
        key = (partition_key, item)
        if key not in self._items:
            raise CosmosResourceNotFoundError(
                message=f"Resource {item!r} not found", response=None,
            )
        return self._stored(self._items[key])

    async def replace_item(
        self,
        item: str,
        body: dict[str, Any],
        *,
        etag: str | None = None,
        match_condition: MatchConditions | None = None,
    ) -> dict[str, Any]:
        pk = self._pk_value(body)
        key = (pk, item)
        if key not in self._items:
            raise CosmosResourceNotFoundError(
                message=f"Resource {item!r} not found", response=None,
            )
        if match_condition == MatchConditions.IfNotModified:
            if etag is None:
                raise ValueError("IfNotModified requires etag")
            if self._items[key].get("_etag") != etag:
                raise CosmosAccessConditionFailedError(
                    message="Precondition (etag) failed", response=None,
                )
        stored = {**body, "_etag": self._next_etag()}
        self._items[key] = stored
        return self._stored(stored)

    async def upsert_item(self, body: dict[str, Any]) -> dict[str, Any]:
        pk = self._pk_value(body)
        doc_id = body["id"]
        stored = {**body, "_etag": self._next_etag()}
        self._items[(pk, doc_id)] = stored
        return self._stored(stored)

    def query_items(
        self,
        query: str,
        parameters: list[dict[str, Any]] | None = None,
    ) -> AsyncIterator[dict[str, Any]]:
        """Mirror of `ContainerProxy.query_items`. Returns an async iterator.

        We only evaluate a tiny SQL subset — enough for our actual queries.
        Recognized forms: SELECT * FROM c [WHERE <cond> [AND <cond> ...]]
        [ORDER BY c.<field> ASC|DESC]. Conditions: `c.<field> = @p`,
        `c.<field> < @p`, `c.<field> > @p`, `IS_DEFINED(c.<field>)`,
        `c.<field> != null`. Boolean glue: AND only.
        """
        params = {p["name"]: p["value"] for p in (parameters or [])}
        results = list(self._items.values())
        results = _evaluate_query(query, results, params)
        return _AsyncIter(results)


class _AsyncIter:
    def __init__(self, items: list[dict[str, Any]]):
        self._items = items
        self._idx = 0

    def __aiter__(self) -> _AsyncIter:
        return self

    async def __anext__(self) -> dict[str, Any]:
        if self._idx >= len(self._items):
            raise StopAsyncIteration
        item = self._items[self._idx]
        self._idx += 1
        return {**item}


# ─── tiny SQL-ish evaluator ──────────────────────────────────────────────────


_WHERE_CLAUSE = re.compile(
    r"^\s*SELECT\s+\*\s+FROM\s+c\s*"
    r"(?:WHERE\s+(?P<where>.+?))?"
    r"(?:\s+ORDER\s+BY\s+c\.(?P<order_field>\w+)\s+(?P<order_dir>ASC|DESC))?"
    r"\s*$",
    re.IGNORECASE | re.DOTALL,
)


def _evaluate_query(
    query: str,
    rows: list[dict[str, Any]],
    params: dict[str, Any],
) -> list[dict[str, Any]]:
    m = _WHERE_CLAUSE.match(query)
    if not m:
        raise NotImplementedError(f"FakeContainer can't parse: {query!r}")

    where = (m.group("where") or "").strip()
    if where:
        rows = [row for row in rows if _evaluate_where(where, row, params)]

    order_field = m.group("order_field")
    if order_field:
        reverse = m.group("order_dir").upper() == "DESC"
        rows = sorted(rows, key=lambda r: r.get(order_field) or "", reverse=reverse)

    return rows


def _split_top_level(expr: str, *ops: str) -> list[str]:
    """Split `expr` on top-level boundary tokens (any of `ops`, case-insensitive),
    respecting parentheses depth. Returns interleaved [piece, op, piece, op, ...]."""
    pieces: list[str] = []
    depth = 0
    i = 0
    last = 0
    upper = expr.upper()
    while i < len(expr):
        c = expr[i]
        if c == "(":
            depth += 1
            i += 1
            continue
        if c == ")":
            depth -= 1
            i += 1
            continue
        if depth == 0 and c.isspace():
            for op in ops:
                op_u = op.upper()
                # boundary check: surrounded by whitespace / start / end
                end = i + 1 + len(op_u)
                if (upper[i + 1: end] == op_u
                        and (end == len(expr) or expr[end].isspace())):
                    pieces.append(expr[last:i].strip())
                    pieces.append(op_u)
                    i = end
                    last = i
                    break
            else:
                i += 1
                continue
            continue
        i += 1
    pieces.append(expr[last:].strip())
    return pieces


def _evaluate_where(where: str, row: dict[str, Any], params: dict[str, Any]) -> bool:
    # AND binds tighter than OR; split on OR first, each disjunct splits on AND.
    or_parts = _split_top_level(where, "OR")
    if len(or_parts) > 1:
        # _split_top_level returns [piece, "OR", piece, "OR", ...]
        for i in range(0, len(or_parts), 2):
            if _evaluate_where(or_parts[i], row, params):
                return True
        return False

    and_parts = _split_top_level(where, "AND")
    if len(and_parts) > 1:
        for i in range(0, len(and_parts), 2):
            if not _evaluate_where(and_parts[i], row, params):
                return False
        return True

    return _evaluate_cond(where, row, params)


def _evaluate_cond(cond: str, row: dict[str, Any], params: dict[str, Any]) -> bool:
    cond = cond.strip()

    # Strip surrounding parens (recursively, in case of nested redundant ones)
    while cond.startswith("(") and cond.endswith(")"):
        # Check that the parens are matched (not just outer-incidental ones)
        depth = 0
        outer = True
        for i, c in enumerate(cond):
            if c == "(":
                depth += 1
            elif c == ")":
                depth -= 1
                if depth == 0 and i < len(cond) - 1:
                    outer = False
                    break
        if not outer:
            break
        cond = cond[1:-1].strip()
        # If the result has top-level AND/OR, recurse into _evaluate_where
        if _split_top_level(cond, "AND", "OR").__len__() > 1:
            return _evaluate_where(cond, row, params)

    # IS_DEFINED(c.field)
    m = re.match(r"^IS_DEFINED\(c\.(\w+)\)$", cond, re.IGNORECASE)
    if m:
        return m.group(1) in row

    # NOT IS_DEFINED(c.field)
    m = re.match(r"^NOT\s+IS_DEFINED\(c\.(\w+)\)$", cond, re.IGNORECASE)
    if m:
        return m.group(1) not in row

    # c.field <op> @param
    m = re.match(r"^c\.(\w+)\s*(=|!=|<|>|<=|>=)\s*(@\w+|null|true|false)$", cond, re.IGNORECASE)
    if m:
        field, op, rhs = m.group(1), m.group(2), m.group(3)
        actual = row.get(field)
        if rhs == "null":
            expected: Any = None
        elif rhs.lower() == "true":
            expected = True
        elif rhs.lower() == "false":
            expected = False
        elif rhs.startswith("@"):
            expected = params[rhs]
        else:
            raise NotImplementedError(f"unsupported rhs: {rhs!r}")
        if op == "=":
            return actual == expected
        if op == "!=":
            return actual != expected
        if op == "<":
            return actual is not None and actual < expected
        if op == ">":
            return actual is not None and actual > expected
        if op == "<=":
            return actual is not None and actual <= expected
        if op == ">=":
            return actual is not None and actual >= expected

    raise NotImplementedError(f"FakeContainer can't evaluate condition: {cond!r}")


# ─── time injection ──────────────────────────────────────────────────────────


class FrozenClock:
    """A controllable clock for tests. Use `set` to advance."""

    def __init__(self, start: datetime | None = None):
        self._now = start or datetime(2026, 5, 1, 12, 0, 0, tzinfo=UTC)

    def now(self) -> datetime:
        return self._now

    def advance(self, *, seconds: float = 0.0) -> None:
        from datetime import timedelta
        self._now = self._now + timedelta(seconds=seconds)

    def set(self, t: datetime) -> None:
        self._now = t

    def as_factory(self) -> Callable[[], datetime]:
        return self.now
