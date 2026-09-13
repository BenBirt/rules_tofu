#!/usr/bin/env bash
#
# Builds the release tarball and prints release notes to stdout. Invoked by the
# bazel-contrib/.github release_ruleset reusable workflow with the tag as $1.
#
# Archive exclusions live HERE (not in .gitattributes) so that ordinary
# `git archive` runs are unaffected. We drop `examples/`, `tests/` and
# `toolchain/tests/`: they are not needed by consumers and reference repos that
# are dev-only or otherwise unavailable when rules_tofu is consumed as a
# dependency. Note the pathspecs are prefix-anchored, so a tests package
# nested under a shipped directory needs its own exclusion. Everything
# else is kept -- crucially `e2e/` and `.bcr/`, which BCR reads from the
# extracted archive (the test module at e2e/smoke and the source/metadata
# templates).
set -o errexit -o nounset -o pipefail

# Tag, e.g. v0.1.0. The prefix strips the leading 'v' so it matches
# strip_prefix ("{REPO}-{VERSION}") in .bcr/source.template.json, and mirrors
# the layout GitHub uses for source archives.
TAG=$1
PREFIX="rules_tofu-${TAG:1}"
ARCHIVE="rules_tofu-$TAG.tar.gz"

git archive --format=tar --prefix="${PREFIX}/" "${TAG}" \
    -- . ':(exclude)examples' ':(exclude)tests' ':(exclude)toolchain/tests' | gzip > "$ARCHIVE"

cat << EOF
## Using Bzlmod

Add to your \`MODULE.bazel\`:

\`\`\`starlark
bazel_dep(name = "rules_tofu", version = "${TAG:1}")

tofu = use_extension("@rules_tofu//toolchain:extensions.bzl", "tofu")
use_repo(tofu, "tofu_toolchains")
\`\`\`
EOF
