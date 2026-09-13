"""Resolution of the single OpenTofu version the toolchain extension pins.

Split out of `extensions.bzl` so the decision is a pure function that
//toolchain/tests can exercise without evaluating a module graph. Problems
are reported as an `error` string on the returned struct rather than raised
with `fail()`: a `fail()` inside this helper would be unobservable from a
Starlark test, and the extension is the only caller entitled to abort the
build. The rules themselves are documented in `extensions.bzl`.
"""

visibility(["//toolchain/..."])

def resolve_version(requests, default):
    """Pick the OpenTofu version for the single globally registered toolchain.

    Args:
        requests: A list of `struct(version, is_root, module_name)`, one per
            `tofu.version` tag across the whole module graph.
        default: Version to use when no module requested one.

    Returns:
        `struct(version, error)`. On success `error` is `None`; on failure
        `version` is `None` and `error` is the message to `fail()` with.
    """
    root_requests = [r for r in requests if r.is_root]

    # One toolchain means one version, so two root tags are a mistake in the
    # root's own MODULE.bazel rather than something to last-write-wins.
    if len(root_requests) > 1:
        return struct(
            version = None,
            error = (
                "Root module declares tofu.version more than once (requested {}). ".format(
                    ", ".join(["`{}`".format(r.version) for r in root_requests]),
                ) +
                "rules_tofu registers a single global OpenTofu toolchain; " +
                "declare exactly one `tofu.version()` tag."
            ),
        )

    # The root module owns the tool its builds run: a dependency's request is
    # a preference, never a constraint on the root.
    if root_requests:
        return struct(version = root_requests[0].version, error = None)

    if not requests:
        return struct(version = default, error = None)

    # With no root tag, honour the dependencies' explicit need rather than
    # quietly substituting the default -- but only where they agree.
    requesters = {}
    for r in requests:
        requesters[r.version] = requesters.get(r.version, []) + [r.module_name]

    versions = sorted(requesters.keys())
    if len(versions) == 1:
        return struct(version = versions[0], error = None)

    detail = "; ".join([
        "`{}` (requested by {})".format(v, ", ".join(sorted(requesters[v])))
        for v in versions
    ])
    return struct(
        version = None,
        error = (
            "Modules request conflicting OpenTofu versions: {}. ".format(detail) +
            "rules_tofu registers a single global OpenTofu toolchain and cannot " +
            "satisfy both; pin the version you want with " +
            "`tofu.version(version = \"...\")` in your root MODULE.bazel."
        ),
    )
