"""Unit tests for `resolve_version` in //toolchain:version_resolution.bzl.

These live in a subpackage rather than in //toolchain so the `@bazel_skylib`
load -- a dev-only dependency of rules_tofu -- stays out of
//toolchain:BUILD.bazel, which every downstream module loads to reach
`:toolchain_type`. The resolution rules under test are stated in
//toolchain:extensions.bzl.
"""

load("@bazel_skylib//lib:unittest.bzl", "asserts", "unittest")
load("//toolchain:version_resolution.bzl", "resolve_version")

# Deliberately not a real OpenTofu version: a test that accidentally falls
# back to the default cannot then look like a test that resolved something.
_DEFAULT = "0.0.0-default"

def _request(version, is_root = False, module_name = "some_dep"):
    return struct(version = version, is_root = is_root, module_name = module_name)

def _no_tags_uses_default_test_impl(ctx):
    env = unittest.begin(ctx)

    resolved = resolve_version([], _DEFAULT)

    asserts.equals(env, None, resolved.error)
    asserts.equals(env, _DEFAULT, resolved.version)
    return unittest.end(env)

_no_tags_uses_default_test = unittest.make(_no_tags_uses_default_test_impl)

def _root_tag_wins_test_impl(ctx):
    env = unittest.begin(ctx)

    resolved = resolve_version(
        [
            _request("1.8.5", module_name = "dep_a"),
            _request("1.12.0", is_root = True, module_name = "root"),
            _request("1.8.5", module_name = "dep_b"),
        ],
        _DEFAULT,
    )

    asserts.equals(env, None, resolved.error)
    asserts.equals(env, "1.12.0", resolved.version)
    return unittest.end(env)

_root_tag_wins_test = unittest.make(_root_tag_wins_test_impl)

def _duplicate_root_tags_fail_test_impl(ctx):
    env = unittest.begin(ctx)

    resolved = resolve_version(
        [
            _request("1.12.0", is_root = True, module_name = "root"),
            _request("1.8.5", is_root = True, module_name = "root"),
        ],
        _DEFAULT,
    )

    asserts.equals(env, None, resolved.version)
    asserts.true(
        env,
        # The fragment //tests/integration asserts on in the nested build.
        "declares tofu.version more than once" in resolved.error,
        "expected the duplicate-root-tag wording, got: {}".format(resolved.error),
    )
    asserts.true(
        env,
        "1.12.0" in resolved.error and "1.8.5" in resolved.error,
        "expected both requested versions to be named, got: {}".format(resolved.error),
    )
    return unittest.end(env)

_duplicate_root_tags_fail_test = unittest.make(_duplicate_root_tags_fail_test_impl)

def _agreeing_non_root_requests_honoured_test_impl(ctx):
    env = unittest.begin(ctx)

    resolved = resolve_version(
        [
            _request("1.8.5", module_name = "dep_a"),
            _request("1.8.5", module_name = "dep_b"),
        ],
        _DEFAULT,
    )

    asserts.equals(env, None, resolved.error)
    asserts.equals(env, "1.8.5", resolved.version)
    return unittest.end(env)

_agreeing_non_root_requests_honoured_test = unittest.make(
    _agreeing_non_root_requests_honoured_test_impl,
)

def _conflicting_non_root_requests_fail_test_impl(ctx):
    env = unittest.begin(ctx)

    resolved = resolve_version(
        [
            _request("1.8.5", module_name = "dep_a"),
            _request("1.12.0", module_name = "dep_b"),
        ],
        _DEFAULT,
    )

    asserts.equals(env, None, resolved.version)
    asserts.true(
        env,
        "conflicting OpenTofu versions" in resolved.error,
        "expected the conflict wording, got: {}".format(resolved.error),
    )
    for named in ["1.8.5", "1.12.0", "dep_a", "dep_b"]:
        asserts.true(
            env,
            named in resolved.error,
            "expected `{}` to be named in: {}".format(named, resolved.error),
        )
    asserts.true(
        env,
        "tofu.version" in resolved.error,
        "expected the message to point at `tofu.version()`, got: {}".format(resolved.error),
    )
    return unittest.end(env)

_conflicting_non_root_requests_fail_test = unittest.make(
    _conflicting_non_root_requests_fail_test_impl,
)

def version_resolution_test_suite(name):
    """Declares the `resolve_version` unit tests.

    Args:
        name: Name of the test_suite gathering the generated test targets.
    """
    unittest.suite(
        name,
        _no_tags_uses_default_test,
        _root_tag_wins_test,
        _duplicate_root_tags_fail_test,
        _agreeing_non_root_requests_honoured_test,
        _conflicting_non_root_requests_fail_test,
    )
