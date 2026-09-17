"""Decides how one node publishes the scope's starter configuration.

Usage: fleet-publish.py PUBLISHED BODY PATCH

PUBLISHED is the ConfigMap the cluster holds now, BODY the one this node
derived from its package. On success PATCH holds the merge patch to apply
(empty when the published configuration already matches); any refusal exits
non-zero with the reason.

A scope carries one package at a time. A node extends the published
configuration only with backends it lacks and never rewrites a backend of the
same package and scope. It replaces the whole configuration only when the
installer has approved exactly this node's package and scope as the
replacement (the desired-* annotations); a node still running the previous
package can therefore never publish over the new one, and an unapproved
package change is refused.
"""
import json
import sys

PACKAGE = "celln.sympozium.ai/package"
SCOPE = "celln.sympozium.ai/scope"
DESIRED_PACKAGE = "celln.sympozium.ai/desired-package"
DESIRED_SCOPE = "celln.sympozium.ai/desired-scope"
PUBLISHED_BY = "celln.sympozium.ai/published-by"


def normalise(data):
    # Configurations published before backends were named carry unprefixed
    # keys for the single backend named native.
    return {k if k.count(".") > 1 else f"native.{k}": v for k, v in data.items()}


def decide(published, body):
    meta = published.get("metadata", {})
    annotations = meta.get("annotations") or {}
    raw = published.get("data") or {}
    ours = body["data"]
    ours_meta = body["metadata"]["annotations"]
    package, scope = ours_meta[PACKAGE], ours_meta[SCOPE]
    current_package, current_scope = annotations.get(PACKAGE), annotations.get(SCOPE)

    if current_package == package and current_scope in (None, scope):
        existing = normalise(raw)
        for key, value in ours.items():
            if key in existing and existing[key] != value:
                sys.exit(
                    f"published starter configuration differs from this node's package for {key}, "
                    f"although both carry package {package}; a node must derive identical files from one package")
        missing = {key: value for key, value in ours.items() if key not in existing}
        patch = {}
        if missing:
            patch["data"] = missing
        if current_scope is None:
            patch["metadata"] = {"annotations": {SCOPE: scope}}
        return patch

    if annotations.get(DESIRED_PACKAGE) == package and annotations.get(DESIRED_SCOPE) == scope:
        data = {key: None for key in raw if key not in ours}
        data.update(ours)
        return {
            "metadata": {
                # Optimistic concurrency: two nodes replacing at once conflict
                # instead of interleaving.
                "resourceVersion": meta.get("resourceVersion"),
                "annotations": {PACKAGE: package, SCOPE: scope, PUBLISHED_BY: ours_meta[PUBLISHED_BY]},
            },
            "data": data,
        }

    desired = annotations.get(DESIRED_PACKAGE)
    if desired and desired != package:
        sys.exit(
            f"the scope is moving to package {desired} but this node runs {package}; "
            "it waits for its configure pod to be replaced")
    sys.exit(
        f"the scope's starter configuration comes from package {current_package} "
        f"(scope {current_scope or 'unrecorded'}) but this node runs package {package} in scope {scope}; "
        "rerun sympozium install with --celln-fleet-replace-package to move the scope to this package "
        "(live parents are lost), or pin the installed package with --celln-fleet-package-image, "
        "--celln-fleet-package-hash and --celln-fleet-publisher")


def main():
    published_path, body_path, patch_path = sys.argv[1:]
    with open(published_path) as f:
        published = json.load(f)
    with open(body_path) as f:
        body = json.load(f)
    patch = decide(published, body)
    with open(patch_path, "w") as out:
        out.write(json.dumps(patch) if patch else "")


if __name__ == "__main__":
    main()
