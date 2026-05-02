"""HTTP entrypoint — streamable-http transport, no incoming auth.

Auth is handled by kube-rbac-proxy in front of this process: clients present
a K8s SA token, the proxy validates it via TokenReview + SubjectAccessReview,
and only authorized requests reach this server. Binding loopback so direct
pod-IP:8080 access bypasses nothing — only the proxy can talk to us.

Outgoing glimmung auth is the pod's own projected SA token, presented as a
bearer to glimmung. Glimmung validates via TokenReview against the cluster
API and checks `mcp-glimmung/mcp-glimmung` against its K8S_SA_ALLOWLIST.
"""

import logging
import os
from contextlib import asynccontextmanager

from mcp.server.fastmcp import FastMCP
from mcp.server.transport_security import TransportSecuritySettings
from starlette.applications import Starlette
from starlette.requests import Request
from starlette.responses import Response
from starlette.routing import Mount, Route

from .glimmung_client import GlimmungClient
from .tools import register_tools


def build_app() -> Starlette:
    # streamable_http ships DNS-rebinding-protection middleware that 421s any
    # Host header not in `allowed_hosts`. Default whitelist only covers
    # localhost, so in-cluster requests to mcp-glimmung.mcp-glimmung.svc get
    # rejected. Disable here — kube-rbac-proxy in front already gates auth
    # via K8s SA tokens. streamable_http_path="/" so POSTs to "/" don't hit
    # Starlette's trailing-slash redirect (was 307 → 421 loop in mcp-github).
    mcp = FastMCP(
        "glimmung-mcp",
        stateless_http=True,
        streamable_http_path="/",
        transport_security=TransportSecuritySettings(
            enable_dns_rebinding_protection=False,
        ),
    )
    base_url = os.environ.get("GLIMMUNG_BASE_URL", "http://glimmung.glimmung.svc")
    register_tools(mcp, GlimmungClient(base_url=base_url))

    async def healthz(_: Request) -> Response:
        return Response("ok", media_type="text/plain")

    # Mount doesn't forward lifespan to the inner app, so FastMCP's
    # session_manager.run() — which sets up the anyio task group the
    # streamable-http handler depends on — never fires when mounted. Wire
    # the run() context into the outer app's lifespan ourselves; without
    # this every request 500s with "Task group is not initialized".
    @asynccontextmanager
    async def lifespan(_app: Starlette):
        async with mcp.session_manager.run():
            yield

    return Starlette(
        routes=[
            Route("/healthz", healthz),
            Mount("/", app=mcp.streamable_http_app()),
        ],
        lifespan=lifespan,
    )


def main() -> None:
    logging.basicConfig(level=logging.INFO)
    import uvicorn

    port = int(os.environ.get("PORT", "8080"))
    uvicorn.run(build_app(), host="127.0.0.1", port=port)


if __name__ == "__main__":
    main()
