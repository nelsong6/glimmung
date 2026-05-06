from glimmung.models import (
    Report,
    ReportReview,
    ReportReviewState,
    ReportState,
    ReportVersion,
    Touchpoint,
    TouchpointReview,
    TouchpointReviewState,
    TouchpointState,
    TouchpointVersion,
)


def test_touchpoint_names_alias_legacy_report_models():
    assert Touchpoint is Report
    assert TouchpointVersion is ReportVersion
    assert TouchpointState is ReportState
    assert TouchpointReview is ReportReview
    assert TouchpointReviewState is ReportReviewState
