"""`tf_deploy` rule + macro.

The macro always emits:
  - `:<name>`         — the `_tf_deploy` data target. Its outputs are
                        the materialized working tree (a symlink-mirror of
                        every transitive .tf input at its workspace-relative
                        path, plus a generated `rules_tofu.auto.tfvars.json`),
                        a validate stamp produced by running
                        `tofu init -backend=false && tofu validate` at build
                        time, and (when `var_files` is non-empty) a stamp
                        produced by a duplicate-variable-key check across
                        `vars` and every `var_files` entry. So
                        `bazel build :<name>` exercises both checks.
  - `:<name>.plan`    — runnable: `bazel run :<name>.plan`
  - `:<name>.apply`   — runnable: `bazel run :<name>.apply`
  - `:<name>.destroy` — runnable: `bazel run :<name>.destroy`
  - `:<name>.fmt`     — runnable: `bazel run :<name>.fmt`

When `fmt_test=True` (the default) and `srcs` is non-empty:
  - `:<name>.fmt_check`  — test: `bazel test :<name>.fmt_check`

Materialization happens at analysis time via `ctx.actions.symlink` (one
action per file). At runtime the runner cd's into the work tree and
invokes `tofu init && tofu <plan|apply>` against it. Nothing is
mktemp'd; nothing is symlinked from bash.
"""

load("//toolchain:toolchain.bzl", "TOOLCHAIN_TYPE")
load(":fmt.bzl", _tf_fmt = "tf_fmt", _tf_fmt_check = "tf_fmt_check_test")
load(":init_action.bzl", _tf_init_validate = "tf_init_validate")
load(":providers.bzl", "TfDeployInfo", "TfLibraryInfo", "TfProviderInfo")
load(":runner.bzl", _tf_runner = "tf_runner")
load(":var_files_check.bzl", "DUPCHECK_BIN", _tf_check_var_files = "tf_check_var_files")
load(
    ":work_tree.bzl",
    "ALLOWED_SRC_EXTS",
    _materialize = "materialize",
    _materialize_plugin_tree = "materialize_plugin_tree",
    _plugin_tree_root = "plugin_tree_root",
    _work_tree_root = "work_tree_root",
)

def _tf_deploy_impl(ctx):
    direct = [struct(path = f.short_path, file = f) for f in ctx.files.srcs + ctx.files.data]
    var_file_entries = [struct(path = f.short_path, file = f) for f in ctx.files.var_files]
    transitive = [d[TfLibraryInfo].transitive_files for d in ctx.attr.deps]
    entries = depset(direct = direct + var_file_entries, transitive = transitive).to_list()

    tfvars_content = json.encode_indent(
        ctx.attr.vars,
        indent = "  ",
    )

    outputs = _materialize(ctx, entries, tfvars_content = tfvars_content)

    # Aggregate providers: direct + transitive via library deps.
    direct_providers = [p[TfProviderInfo] for p in ctx.attr.providers]
    transitive_provider_sets = [
        d[TfLibraryInfo].providers
        for d in ctx.attr.deps
    ]
    providers_depset = depset(direct = direct_providers, transitive = transitive_provider_sets)
    plugin_outputs = _materialize_plugin_tree(ctx, providers_depset)
    outputs = outputs + plugin_outputs

    work_tree_files = depset(direct = outputs)

    validate_stamp = _tf_init_validate(
        ctx,
        work_tree_files = work_tree_files,
        work_tree_root = _work_tree_root(ctx),
        package_dir = ctx.label.package,
        plugin_tree_root = _plugin_tree_root(ctx),
    )

    var_files_check_stamp = _tf_check_var_files(
        ctx,
        vars_keys = sorted(ctx.attr.vars.keys()),
        var_files = ctx.files.var_files,
    )

    default_files = [validate_stamp]
    if var_files_check_stamp != None:
        default_files.append(var_files_check_stamp)

    return [
        DefaultInfo(files = depset(direct = default_files, transitive = [work_tree_files])),
        TfDeployInfo(
            work_tree_files = work_tree_files,
            package_dir = ctx.label.package,
            var_file_relpaths = [f.short_path for f in ctx.files.var_files],
        ),
    ]

tf_deploy_rule = rule(
    implementation = _tf_deploy_impl,
    attrs = {
        "srcs": attr.label_list(
            allow_files = ALLOWED_SRC_EXTS,
            doc = "Optional deploy-local config files (e.g. provider/backend setup).",
        ),
        "deps": attr.label_list(
            providers = [TfLibraryInfo],
            doc = "`tf_library` targets this deploy composes.",
        ),
        "vars": attr.string_dict(
            doc = "Variable values bound to this deploy. Rendered to rules_tofu.auto.tfvars.json.",
        ),
        "var_files": attr.label_list(
            allow_files = [".tfvars.json", ".tfvars"],
            doc = "Variable-value files passed to every tofu invocation via -var-file. " +
                  "Accepts any Label producing a .tfvars.json or .tfvars file (e.g. a genrule output). " +
                  "Keys in var_files must not overlap with keys in vars or other var_files entries.",
        ),
        "data": attr.label_list(
            allow_files = True,
            doc = "Arbitrary files to include in the work tree. " +
                  "Use to expose files for `file()` calls in Terraform configs.",
        ),
        "providers": attr.label_list(
            providers = [TfProviderInfo],
            doc = "Vendored OpenTofu/Terraform providers this deploy declares directly. " +
                  "Unioned with providers contributed transitively by `deps`. " +
                  "Each entry is typically a `@<repo>//:provider` target exposed by " +
                  "`tf_providers.provider(...)` in MODULE.bazel.",
        ),
        "_dupcheck_bin": attr.label(
            default = DUPCHECK_BIN,
            executable = True,
            cfg = "exec",
        ),
    },
    toolchains = [TOOLCHAIN_TYPE],
    doc = "Underlying data-carrier for `tf_deploy`. Use the macro.",
)

def tf_deploy(name, srcs = None, deps = None, vars = None, var_files = None, data = None, providers = None, fmt_test = True, **kwargs):
    """A root Terraform/OpenTofu invocation.

    Always generates:
      - `:<name>`         — data target. Building it materializes the work tree
                            and runs `tofu init -backend=false && tofu validate`
                            against it, so `bazel build :<name>` validates.
      - `:<name>.plan`    — `bazel run` to produce a plan.
      - `:<name>.apply`   — `bazel run` to apply.
      - `:<name>.destroy` — `bazel run` to destroy all managed resources.
      - `:<name>.fmt`     — `bazel run` to reformat .tf files in-place.

    When `fmt_test=True` (the default) and `srcs` is non-empty:
      - `:<name>.fmt_check` — `bazel test` that fails if files are not formatted.

    Args:
      name: target name.
      srcs: optional deploy-local .tf files (e.g. provider/backend config).
      deps: `tf_library` targets to compose.
      vars: dict of variable name -> value, rendered to rules_tofu.auto.tfvars.json.
      var_files: Labels producing .tfvars.json or .tfvars files passed to tofu via -var-file.
          Accepts any Bazel Label (e.g. a genrule output). Keys must not overlap
          with vars or other var_files entries; duplicate keys fail at
          `bazel build` time via a dedicated check action.
      data: arbitrary files to include in the work tree, enabling `file()` calls
          in Terraform configs.
      providers: `tf_provider` targets (typically `@<repo>//:provider` exposed
          by `tf_providers.provider(...)` in MODULE.bazel) declared at the deploy
          level. Unioned with providers transitively contributed by `deps`. The exec
          platform binary is symlinked into a sibling plugin tree; `tofu init`
          runs offline against it.
      fmt_test: whether to emit a `:<name>.fmt_check` test target. Only emitted
          when True (the default) and `srcs` is non-empty.
      **kwargs: forwarded to the underlying rule (visibility, tags, testonly).
    """
    common_kwargs = {}
    for forwarded in ("visibility", "tags", "testonly"):
        if forwarded in kwargs:
            common_kwargs[forwarded] = kwargs.pop(forwarded)

    tf_deploy_rule(
        name = name,
        srcs = srcs or [],
        deps = deps or [],
        vars = vars or {},
        var_files = var_files or [],
        data = data or [],
        providers = providers or [],
        **dict(common_kwargs, **kwargs)
    )

    _tf_runner(
        name = name + ".plan",
        deploy = ":" + name,
        command = "plan",
        **common_kwargs
    )

    _tf_runner(
        name = name + ".apply",
        deploy = ":" + name,
        command = "apply",
        **common_kwargs
    )

    _tf_runner(
        name = name + ".destroy",
        deploy = ":" + name,
        command = "destroy",
        **common_kwargs
    )

    _tf_fmt(name = name + ".fmt", **common_kwargs)
    if fmt_test and srcs:
        _tf_fmt_check(
            name = name + ".fmt_check",
            srcs = srcs,
            **common_kwargs
        )
