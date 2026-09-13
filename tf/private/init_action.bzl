"""Build-time `tofu init -backend=false` + `tofu validate` for a materialized
work tree.

The deploy rule (and the library's auto-emitted validating deploy) call
`tf_init_validate(...)` to attach a validate stamp to the rule's outputs, so
`bazel build :foo` exercises validation in the build graph (with caching).

`-plugin-dir` is always passed (pointing at the sibling plugin tree that
`materialize_plugin_tree(...)` declares), so init is offline regardless of
whether `required_providers` is declared. With zero providers in scope the
plugin tree has no files, so the action falls back to an empty directory and
init is still a no-op for provider installation.
"""

load("//toolchain:toolchain.bzl", "TOOLCHAIN_TYPE")

def tf_init_validate(ctx, work_tree_files, work_tree_root, package_dir, plugin_tree_root):
    """Emit a build-time `tofu init -backend=false` + `tofu validate` action.

    Args:
      ctx: rule ctx. Must list `TOOLCHAIN_TYPE` in `toolchains`.
      work_tree_files: depset[File] of every materialized work tree file,
          plus the sibling plugin tree's provider binaries (read in place via
          `-plugin-dir`, so they must still be listed as action inputs).
      work_tree_root: string, exec-root-relative path to the work tree root,
          e.g. `<bin>/<pkg>/<name>.work`.
      package_dir: string, workspace-relative directory inside the work tree
          that tofu should cd into before running init/validate.
      plugin_tree_root: string, exec-root-relative path to the sibling plugin
          tree root, e.g. `<bin>/<pkg>/<name>.plugins`. Pinned via
          `-plugin-dir` so init runs offline.

    Returns:
      File: a stamp file declared as an action output.
    """
    tofu = ctx.toolchains[TOOLCHAIN_TYPE].tofu
    stamp = ctx.actions.declare_file(ctx.label.name + ".validate.stamp")

    # tofu init writes `.terraform/` and `.terraform.lock.hcl` next to the
    # config. The materialized work tree directory in the sandbox contains
    # symlinks (Bazel-managed); to avoid any chance of conflicting with
    # sandbox read-only enforcement on input trees, copy the work tree into
    # a scratch dir under $TMPDIR (dereferencing symlinks) and run there.
    # Only the (small) config tree is copied — the provider binaries live in
    # the sibling plugin tree and are read in place via -plugin-dir, never
    # copied. Bazel cleans up the action's sandbox on exit, so the copy is
    # throwaway — no explicit `rm -rf` needed.
    ctx.actions.run_shell(
        inputs = depset(direct = [tofu.binary], transitive = [work_tree_files]),
        outputs = [stamp],
        command = """\
set -euo pipefail
TOFU=$1
WORK_TREE_ROOT=$2
PACKAGE_DIR=$3
PLUGIN_TREE_ROOT=$4
STAMP=$5
# Resolve the input plugin tree to an absolute path up front: tofu resolves a
# relative -plugin-dir against the -chdir directory, and $PWD here is the
# action's exec root. The script never cd's, so capturing $PWD inline is safe.
PLUGIN_DIR="$PWD/$PLUGIN_TREE_ROOT"
SCRATCH=$(mktemp -d "${TMPDIR:-/tmp}/rules_tofu-validate-XXXXXX")
cp -RL "$WORK_TREE_ROOT"/. "$SCRATCH"/
# Zero-providers case: no files are declared under the plugin root, so its
# input directory is absent from the sandbox. Fall back to an empty dir so
# init runs offline and installs nothing.
if [ ! -d "$PLUGIN_DIR" ]; then
    PLUGIN_DIR="$SCRATCH/.rules_tofu-empty-plugins"
    mkdir -p "$PLUGIN_DIR"
fi
CWD="$SCRATCH/$PACKAGE_DIR"
if [ -e "$CWD/.terraform.lock.hcl" ]; then
    echo "rules_tofu: refusing to validate: .terraform.lock.hcl present in work tree under $PACKAGE_DIR. " \\
         "Lock files are managed implicitly via Bazel's provider pinning; remove it from srcs/data." 1>&2
    exit 1
fi
# Redirect init stdout to suppress "Installing provider" progress spam.
# Stderr is kept so error messages (e.g. "Failed to query available provider
# packages") and the "Incomplete lock file" warning remain visible.
# There is no flag to skip lockfile generation — the lockfile is written into
# $SCRATCH and discarded with it.
"$TOFU" -chdir="$CWD" init -backend=false -input=false -plugin-dir="$PLUGIN_DIR" >/dev/null
"$TOFU" -chdir="$CWD" validate
touch "$STAMP"
""",
        arguments = [
            tofu.binary.path,
            work_tree_root,
            package_dir,
            plugin_tree_root,
            stamp.path,
        ],
        env = {"TF_IN_AUTOMATION": "1"},
        mnemonic = "TofuValidate",
        progress_message = "TofuValidate %{label}",
    )
    return stamp
