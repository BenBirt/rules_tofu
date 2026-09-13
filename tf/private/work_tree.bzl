"""Work tree materialization shared between `tf_library` and
`tf_deploy`.

A "work tree" is the directory under `bazel-bin/<pkg>/<name>.work/` into
which a rule symlinks every transitive `.tf`/data file at its
workspace-relative path, plus (for deploys) a generated
`rules_tofu.auto.tfvars.json`. The vendored provider plugin tree lives in a
sibling directory, `bazel-bin/<pkg>/<name>.plugins/`, so `tofu init
-plugin-dir=<plugin_tree>` reads the providers in place without the work
tree ever carrying the (large) provider binaries.

`materialize(...)` handles the .tf/data half (with optional tfvars).
`materialize_plugin_tree(...)` handles the per-provider symlinks.
"""

load("//toolchain:toolchain.bzl", "TOOLCHAIN_TYPE")

# Only structural Terraform inputs are allowed in a rule's srcs. Variable
# values come from `tf_deploy(vars = {...})`; allowing `.tfvars[.json]`
# here would create ambiguous precedence with the deploy-emitted
# `rules_tofu.auto.tfvars.json` and lets a reusable module declare values
# it has no business owning.
ALLOWED_SRC_EXTS = [".tf", ".tf.json", ".tftpl", ".hcl"]

def runfiles_path(workspace_name, f):
    """Return the path at which `f` appears in a runfiles tree.

    Convention:
      - Main-repo files: `<workspace_name>/<short_path>`.
      - External-repo files: `<short_path>` with the leading `../` stripped.
    """
    sp = f.short_path
    if sp.startswith("../"):
        return sp[3:]
    return workspace_name + "/" + sp

def materialize(ctx, entries, tfvars_content = None):
    """Materialize a work tree under `<pkg>/<name>.work/`.

    For each `struct(path, file)` in `entries`, declares `<name>.work/<path>`
    and symlinks it to `file`. If `tfvars_content` is non-None, also writes
    `<name>.work/<package>/rules_tofu.auto.tfvars.json` with that content.

    Args:
      ctx: rule ctx.
      entries: list[struct(path, file)] — workspace-relative paths + Files.
      tfvars_content: optional string. Pass None for library work trees
          (which carry no variable values).

    Returns:
      list[File]: the declared work-tree outputs (symlinks plus, when
      `tfvars_content` is set, the generated tfvars file).
    """
    work_prefix = ctx.label.name + ".work"
    outputs = []
    seen = {}
    for entry in entries:
        path = entry.path
        if path.startswith("../"):
            fail(
                "`{}` would include `{}` from an external Bazel module. ".format(
                    ctx.label,
                    entry.file.path,
                ) + "Terraform has no addressing scheme for files outside the workspace " +
                "root, so this is not supported. Bring the file in-workspace (e.g. via " +
                "a `genrule` or a local copy) and depend on that instead.",
            )
        if path in seen:
            other = seen[path]
            if other != entry.file:
                fail(
                    "File collision at workspace path `{}` between `{}` and `{}`. ".format(
                        path,
                        other.path,
                        entry.file.path,
                    ) + "Two libraries are contributing different content at the same path.",
                )
            continue
        seen[path] = entry.file
        out = ctx.actions.declare_file(work_prefix + "/" + path)
        ctx.actions.symlink(output = out, target_file = entry.file)
        outputs.append(out)

    if tfvars_content != None:
        tfvars_path = work_prefix + "/" + ctx.label.package + "/rules_tofu.auto.tfvars.json"
        if tfvars_path[len(work_prefix) + 1:] in seen:
            fail(
                "`{}` collides with the generated rules_tofu.auto.tfvars.json. ".format(
                    seen[tfvars_path[len(work_prefix) + 1:]].path,
                ) + "Rename or remove that file; deploy `vars` is the sole producer of tfvars.",
            )
        tfvars_file = ctx.actions.declare_file(tfvars_path)
        ctx.actions.write(output = tfvars_file, content = tfvars_content)
        outputs.append(tfvars_file)

    return outputs

def materialize_plugin_tree(ctx, providers_depset):
    """Symlink provider binaries into this rule's sibling plugin tree.

    Declares outputs under `<name>.plugins/` (a sibling of the work tree),
    one symlink per unique `(address, version)` using Terraform's canonical
    plugin-dir layout `<host>/<namespace>/<name>/<version>/<os>_<arch>/<binary>`.

    Args:
      ctx: rule ctx. Must list `TOOLCHAIN_TYPE` in `toolchains` so
          `tofu.platform_key` is available.
      providers_depset: depset[TfProviderInfo].

    Returns:
      list[File]: declared symlink outputs (possibly empty).

    Fails if two providers share an address but differ in version, or if a
    declared provider has no binary for the exec platform.
    """
    tofu = ctx.toolchains[TOOLCHAIN_TYPE].tofu
    platform_key = tofu.platform_key
    plugin_prefix = ctx.label.name + ".plugins"

    seen_versions = {}
    outputs = []
    for prov in providers_depset.to_list():
        prior = seen_versions.get(prov.address)
        if prior != None:
            if prior != prov.version:
                fail(
                    ("`{label}` has conflicting versions for provider `{addr}`: " +
                     "`{a}` vs `{b}`. Pick one in MODULE.bazel.").format(
                        label = ctx.label,
                        addr = prov.address,
                        a = prior,
                        b = prov.version,
                    ),
                )
            continue
        seen_versions[prov.address] = prov.version

        binary = prov.binaries.get(platform_key)
        if binary == None:
            fail(
                ("`{label}` requires provider `{addr}@{ver}` for exec platform " +
                 "`{plat}`, but the provider was declared without a `{plat}` entry " +
                 "in its `sha256` map. Add it in MODULE.bazel.").format(
                    label = ctx.label,
                    addr = prov.address,
                    ver = prov.version,
                    plat = platform_key,
                ),
            )

        parts = prov.address.split("/")
        if len(parts) != 3:
            fail("invalid provider address `{}` (expected `<host>/<ns>/<name>`)".format(prov.address))
        target_rel = "{prefix}/{host}/{ns}/{name}/{version}/{plat}/{filename}".format(
            prefix = plugin_prefix,
            host = parts[0],
            ns = parts[1],
            name = parts[2],
            version = prov.version,
            plat = platform_key,
            filename = binary.basename,
        )
        out = ctx.actions.declare_file(target_rel)
        ctx.actions.symlink(output = out, target_file = binary)
        outputs.append(out)

    return outputs

def work_tree_root(ctx):
    """Exec-root-relative path to this rule's work tree root."""
    return "{bin}/{pkg}/{name}.work".format(
        bin = ctx.bin_dir.path,
        pkg = ctx.label.package,
        name = ctx.label.name,
    )

def plugin_tree_root(ctx):
    """Exec-root-relative path to this rule's plugin tree root.

    Sibling of `work_tree_root(ctx)`; the vendored provider binaries
    declared by `materialize_plugin_tree(...)` live under it.
    """
    return "{bin}/{pkg}/{name}.plugins".format(
        bin = ctx.bin_dir.path,
        pkg = ctx.label.package,
        name = ctx.label.name,
    )
