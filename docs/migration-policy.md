# Migration Policy

When migrating off an old system, the old system must be deleted end to end.
Compatibility is prohibited.

This is not a preference or a default that can be overridden by agent caution.
It is the migration contract for this repository.

## Completion Standard

A migrated path is not complete until all of these are true:

- no live routes
- no UI controls or links
- no allocator or executor branches
- no fallback defaults
- no old behavior tests
- no docs saying the old path is supported
- no runtime reads whose purpose is to keep old behavior working

Unknown callers are unsupported. Known old callers are unsupported. Old data
does not justify runtime support.

If removal exposes another dependency on the old system, delete that dependency
too. If the task cannot be completed, stop with a blocker report naming the
exact remaining old dependency.

Do not add a compatibility layer. Do not add a fallback. Do not keep a
read-only runtime path.

## Agent Checklist

When asked to complete a migration, search for the old system's names, routes,
types, feature flags, tests, docs, UI labels, and storage behavior. Remove every
live path.

Treat `legacy`, `compatibility`, `fallback`, `temporary`, and `exception` as
deletion targets, not design options.

Tests should fail if the retired path is reintroduced into live code.
