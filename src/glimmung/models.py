from datetime import datetime
from enum import Enum
from typing import Any

from pydantic import BaseModel, Field


class LeaseState(str, Enum):
    PENDING = "pending"
    ACTIVE = "active"
    RELEASED = "released"
    EXPIRED = "expired"


class Project(BaseModel):
    id: str
    name: str
    github_repo: str = ""             # e.g. "nelsong6/spirelens"
    workflow_filename: str = ""       # e.g. "issue-agent.yaml"
    workflow_ref: str = "main"
    trigger_label: str = "issue-agent"
    default_requirements: dict[str, Any] = Field(default_factory=dict)
    metadata: dict[str, Any] = Field(default_factory=dict)
    created_at: datetime


class ProjectRegister(BaseModel):
    name: str
    github_repo: str
    workflow_filename: str
    workflow_ref: str = "main"
    trigger_label: str = "issue-agent"
    default_requirements: dict[str, Any] = Field(default_factory=dict)


class Host(BaseModel):
    id: str
    name: str
    capabilities: dict[str, Any] = Field(default_factory=dict)
    current_lease_id: str | None = None
    last_heartbeat: datetime | None = None
    last_used_at: datetime | None = None
    drained: bool = False
    created_at: datetime


class Lease(BaseModel):
    id: str
    project: str
    host: str | None = None
    state: LeaseState = LeaseState.PENDING
    requirements: dict[str, Any] = Field(default_factory=dict)
    metadata: dict[str, Any] = Field(default_factory=dict)
    requested_at: datetime
    assigned_at: datetime | None = None
    released_at: datetime | None = None
    ttl_seconds: int = 900


class LeaseRequest(BaseModel):
    project: str
    requirements: dict[str, Any] = Field(default_factory=dict)
    metadata: dict[str, Any] = Field(default_factory=dict)
    ttl_seconds: int | None = None


class LeaseResponse(BaseModel):
    lease: Lease
    host: Host | None = None


class StateSnapshot(BaseModel):
    hosts: list[Host]
    pending_leases: list[Lease]
    active_leases: list[Lease]
    projects: list[Project] = Field(default_factory=list)
