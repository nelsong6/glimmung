from __future__ import annotations

import argparse
import json
import sys

from . import ops


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Glimmung agent-CI orchestration")
    sub = parser.add_subparsers(dest="command", required=True)

    p = sub.add_parser("build-preview-image")
    p.add_argument("--image-tag", required=True)

    p = sub.add_parser("deploy-validation-preview")
    p.add_argument("--release", required=True)
    p.add_argument("--namespace", default=ops.PROD_NAMESPACE)
    p.add_argument("--image-tag", required=True)
    p.add_argument("--public-host", required=True)
    p.add_argument("--pr-number", default="")

    p = sub.add_parser("label-release-pr",
        help="(legacy) Add glimmung.io/pr=<N> labels to the issue release's resources after the PR opens.")
    p.add_argument("--release", required=True)
    p.add_argument("--namespace", default=ops.PROD_NAMESPACE)
    p.add_argument("--pr-number", required=True)

    p = sub.add_parser("label-release-branch",
        help="Add glimmung.io/branch=<slug> labels to the issue release's resources at apply time (#69 path).")
    p.add_argument("--release", required=True)
    p.add_argument("--namespace", default=ops.PROD_NAMESPACE)
    p.add_argument("--branch-slug", required=True)

    p = sub.add_parser("rebuild-validation-image",
        help="Build a fresh image from the agent's pushed branch and roll the issue release onto it.")
    p.add_argument("--release", required=True)
    p.add_argument("--namespace", default=ops.PROD_NAMESPACE)
    p.add_argument("--branch", required=True)
    p.add_argument("--image-tag", required=True)
    p.add_argument("--repo-slug", default=ops.REPO_SLUG_DEFAULT)

    p = sub.add_parser("wait-public-preview")
    p.add_argument("--url", required=True)
    p.add_argument("--timeout-seconds", type=int, default=900)

    p = sub.add_parser("destroy-validation-preview")
    p.add_argument("--release", required=True)
    p.add_argument("--namespace", default=ops.PROD_NAMESPACE)

    p = sub.add_parser("apply-agent-job",
        help="Render and `kubectl apply` the agent Job for one issue run.")
    p.add_argument("--namespace", required=True)
    p.add_argument("--job-name", required=True)
    p.add_argument("--issue-number", default="")
    p.add_argument("--issue-id", default="")
    p.add_argument("--issue-title", required=True)
    p.add_argument("--issue-url", required=True)
    p.add_argument("--validation-url", required=True)
    p.add_argument("--branch-name", required=True)
    p.add_argument("--proxy-ip", required=True)
    p.add_argument("--agent-container-tag", required=True)
    p.add_argument("--repo-slug", default=ops.REPO_SLUG_DEFAULT)

    p = sub.add_parser("wait-agent-job",
        help="Wait for an agent Job's Pod to reach a terminal state, streaming logs.")
    p.add_argument("--namespace", required=True)
    p.add_argument("--job-name", required=True)
    p.add_argument("--timeout-seconds", type=int, default=1800)

    return parser


def dump(result: dict) -> None:
    json.dump(result, sys.stdout, indent=2)
    sys.stdout.write("\n")


def main() -> int:
    args = build_parser().parse_args()
    try:
        if args.command == "build-preview-image":
            dump(ops.build_preview_image(image_tag=args.image_tag))
        elif args.command == "deploy-validation-preview":
            dump(ops.deploy_preview(
                release=args.release,
                namespace=args.namespace,
                image_tag=args.image_tag,
                public_host=args.public_host,
                pr_number=args.pr_number,
            ))
        elif args.command == "label-release-pr":
            dump(ops.label_release_pr(
                release=args.release,
                namespace=args.namespace,
                pr_number=args.pr_number,
            ))
        elif args.command == "label-release-branch":
            dump(ops.label_release_branch(
                release=args.release,
                namespace=args.namespace,
                branch_slug=args.branch_slug,
            ))
        elif args.command == "rebuild-validation-image":
            dump(ops.rebuild_validation_image(
                release=args.release,
                namespace=args.namespace,
                branch=args.branch,
                image_tag=args.image_tag,
                repo_slug=args.repo_slug,
            ))
        elif args.command == "wait-public-preview":
            dump(ops.wait_public_preview(url=args.url, timeout_seconds=args.timeout_seconds))
        elif args.command == "destroy-validation-preview":
            dump(ops.destroy_preview(release=args.release, namespace=args.namespace))
        elif args.command == "apply-agent-job":
            dump(ops.apply_agent_job(
                namespace=args.namespace,
                job_name=args.job_name,
                issue_number=args.issue_number,
                issue_id=args.issue_id,
                issue_title=args.issue_title,
                issue_url=args.issue_url,
                validation_url=args.validation_url,
                branch_name=args.branch_name,
                proxy_ip=args.proxy_ip,
                agent_container_tag=args.agent_container_tag,
                repo_slug=args.repo_slug,
            ))
        elif args.command == "wait-agent-job":
            dump(ops.wait_agent_job(
                namespace=args.namespace,
                job_name=args.job_name,
                timeout_seconds=args.timeout_seconds,
            ))
    except Exception as error:
        print(
            json.dumps(
                {"success": False, "error": f"{type(error).__name__}: {error}", "command": args.command},
                indent=2,
            ),
            file=sys.stderr,
        )
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
