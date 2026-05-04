import os
from datetime import UTC, datetime, timedelta
from types import SimpleNamespace

import pytest

from glimmung.locks import claim_lock, extend_lock, read_lock, release_lock

from .cosmos_fake import FakeContainer, FrozenClock


pytestmark = pytest.mark.skipif(
    os.environ.get("GLIMMUNG_TEST_COSMOS", "").lower() != "live",
    reason="live Cosmos smoke only runs when GLIMMUNG_TEST_COSMOS=live",
)


@pytest.mark.asyncio
async def test_live_cosmos_lock_lifecycle_round_trip():
    cosmos = SimpleNamespace(locks=FakeContainer("locks", "scope"))
    clock = FrozenClock(start=datetime.now(UTC))

    claimed = await claim_lock(
        cosmos, scope="ci-smoke", key="lock", holder_id="ci",
        ttl_seconds=300, now_factory=clock.as_factory(),
    )
    assert claimed.held_by == "ci"

    stored = await read_lock(cosmos, scope="ci-smoke", key="lock")
    assert stored is not None
    assert stored.held_by == "ci"

    clock.advance(seconds=10)
    extended = await extend_lock(
        cosmos, scope="ci-smoke", key="lock", holder_id="ci",
        ttl_seconds=600, now_factory=clock.as_factory(),
    )
    assert extended.expires_at == clock.now() + timedelta(seconds=600)

    assert await release_lock(cosmos, scope="ci-smoke", key="lock", holder_id="ci") is True
