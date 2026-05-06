"""Touchpoint-named API over the legacy `reports` storage module.

The Cosmos container and original model names remain `reports` / `Report`
for compatibility. New orchestration code should import this module so the
product vocabulary is Touchpoint while old callers can keep using
`glimmung.reports`.
"""

from __future__ import annotations

from glimmung.reports import (
    append_report_comment as append_touchpoint_comment,
    append_report_review as append_touchpoint_review,
    close_report as close_touchpoint,
    create_report as create_touchpoint,
    create_report_version as create_touchpoint_version,
    ensure_report_for_github as ensure_touchpoint_for_github,
    find_report_by_repo_number as find_touchpoint_by_repo_number,
    github_pull_request_url_for as github_pull_request_url_for,
    list_active_reports as list_active_touchpoints,
    list_report_versions as list_touchpoint_versions,
    list_reports as list_touchpoints,
    merge_report as merge_touchpoint,
    read_report as read_touchpoint,
    read_report_version as read_touchpoint_version,
    reopen_report as reopen_touchpoint,
    set_report_state as set_touchpoint_state,
    update_report as update_touchpoint,
)

# Compatibility names. Keeping these here lets migrated call sites switch
# modules first, then rename individual operations without a large patch.
append_report_comment = append_touchpoint_comment
append_report_review = append_touchpoint_review
close_report = close_touchpoint
create_report = create_touchpoint
create_report_version = create_touchpoint_version
ensure_report_for_github = ensure_touchpoint_for_github
find_report_by_repo_number = find_touchpoint_by_repo_number
list_active_reports = list_active_touchpoints
list_report_versions = list_touchpoint_versions
list_reports = list_touchpoints
merge_report = merge_touchpoint
read_report = read_touchpoint
read_report_version = read_touchpoint_version
reopen_report = reopen_touchpoint
set_report_state = set_touchpoint_state
update_report = update_touchpoint
