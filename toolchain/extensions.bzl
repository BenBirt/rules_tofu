"""Bzlmod module extension that downloads pinned OpenTofu binaries and exposes
them as a registered toolchain (one entry per supported platform).

Downstream usage in MODULE.bazel:

    tofu = use_extension("@rules_tofu//toolchain:extensions.bzl", "tofu")
    # tofu.version(version = "1.8.5")  # optional override
    use_repo(tofu, "tofu_toolchains")

rules_tofu registers exactly one OpenTofu toolchain for the whole build, so
every `tofu.version` tag in the module graph has to collapse to a single
version. The root module arbitrates:

  1. Two or more `tofu.version` tags in the **root** module → error. There is
     one toolchain; asking for two versions is a mistake in the root's own
     MODULE.bazel, not something to settle by letting the last tag win.
  2. Exactly one root tag → that version wins unconditionally, whatever
     non-root modules ask for. The root owns the tool its builds run.
  3. No root tag and all non-root requests agree → that version. A
     dependency's explicit need is honoured rather than quietly replaced by
     `DEFAULT_VERSION`.
  4. No root tag and non-root modules request different versions → error
     naming both the versions and the modules that asked for them. A single
     global toolchain genuinely cannot satisfy both, and picking one silently
     would bury the disagreement.
  5. No tags anywhere → `DEFAULT_VERSION`.

However it is reached, the resolved version must appear in `KNOWN_VERSIONS`.
The branching lives in //toolchain:version_resolution.bzl so it is
unit-testable without a module graph; this file is the adapter that collects
the tags and owns the `fail()`.
"""

load("//toolchain:version_resolution.bzl", "resolve_version")
load("//toolchain:versions.bzl", "DEFAULT_VERSION", "KNOWN_VERSIONS", "PLATFORMS")

# ---- Per-platform download repository rule -----------------------------------

def _tofu_download_impl(repository_ctx):
    version = repository_ctx.attr.version
    platform_key = repository_ctx.attr.platform_key
    sha256 = repository_ctx.attr.sha256
    exe_suffix = repository_ctx.attr.exe_suffix

    # OpenTofu release asset URL pattern. Asset name uses os_arch (e.g.
    # `tofu_1.8.5_linux_amd64.zip`); our platform_key is already in that form.
    url = "https://github.com/opentofu/opentofu/releases/download/v{v}/tofu_{v}_{p}.zip".format(
        v = version,
        p = platform_key,
    )

    repository_ctx.download_and_extract(
        url = url,
        sha256 = sha256,
        type = "zip",
    )

    binary_name = "tofu" + exe_suffix
    if not repository_ctx.path(binary_name).exists:
        fail("OpenTofu archive did not contain expected binary `{}` (url={})".format(
            binary_name,
            url,
        ))

    repository_ctx.file(
        "BUILD.bazel",
        content = """\
load("@rules_tofu//toolchain:toolchain.bzl", "opentofu_toolchain")

package(default_visibility = ["//visibility:public"])

exports_files(["{binary}"])

opentofu_toolchain(
    name = "toolchain_impl",
    binary = "{binary}",
    version = "{version}",
    platform_key = "{platform_key}",
)
""".format(binary = binary_name, version = version, platform_key = platform_key),
        executable = False,
    )

_tofu_download = repository_rule(
    implementation = _tofu_download_impl,
    attrs = {
        "version": attr.string(mandatory = True),
        "platform_key": attr.string(mandatory = True),
        "sha256": attr.string(mandatory = True),
        "exe_suffix": attr.string(default = ""),
    },
)

# ---- Hub repository that aggregates per-platform toolchain() entries ---------

def _tofu_hub_impl(repository_ctx):
    repository_ctx.file("WORKSPACE", "")
    lines = ["package(default_visibility = [\"//visibility:public\"])", ""]
    for platform_key, os_constraint, cpu_constraint, _ in PLATFORMS:
        lines.extend([
            "toolchain(",
            "    name = \"{}_toolchain\",".format(platform_key),
            "    toolchain = \"@tofu_{}//:toolchain_impl\",".format(platform_key),
            "    toolchain_type = \"@rules_tofu//toolchain:toolchain_type\",",
            "    exec_compatible_with = [",
            "        \"{}\",".format(os_constraint),
            "        \"{}\",".format(cpu_constraint),
            "    ],",
            "    target_compatible_with = [",
            "        \"{}\",".format(os_constraint),
            "        \"{}\",".format(cpu_constraint),
            "    ],",
            ")",
            "",
        ])
    repository_ctx.file("BUILD.bazel", "\n".join(lines), executable = False)

_tofu_hub = repository_rule(
    implementation = _tofu_hub_impl,
)

# ---- Module extension --------------------------------------------------------

_version_tag = tag_class(
    attrs = {
        "version": attr.string(
            doc = "OpenTofu version to pin (must appear in versions.bzl's KNOWN_VERSIONS).",
            mandatory = True,
        ),
    },
)

def _tofu_extension_impl(module_ctx):
    requests = []
    for mod in module_ctx.modules:
        for tag in mod.tags.version:
            requests.append(struct(
                version = tag.version,
                is_root = mod.is_root,
                module_name = mod.name,
            ))

    resolved = resolve_version(requests, DEFAULT_VERSION)
    if resolved.error:
        fail(resolved.error)
    version = resolved.version

    if version not in KNOWN_VERSIONS:
        fail(
            "Unknown OpenTofu version `{}`. Known versions: {}. ".format(
                version,
                sorted(KNOWN_VERSIONS.keys()),
            ) + "Add it to //toolchain:versions.bzl to use a new version.",
        )

    shas = KNOWN_VERSIONS[version]
    for platform_key, _, _, exe_suffix in PLATFORMS:
        if platform_key not in shas:
            fail("No SHA256 recorded for {} at OpenTofu {}".format(platform_key, version))
        _tofu_download(
            name = "tofu_" + platform_key,
            version = version,
            platform_key = platform_key,
            sha256 = shas[platform_key],
            exe_suffix = exe_suffix,
        )

    _tofu_hub(name = "tofu_toolchains")

    # The hub is the only repo a downstream `use_repo`s; the per-platform
    # `tofu_<platform>` repos are an implementation detail the hub's
    # `toolchain()` entries reference by name and must stay off the root's
    # direct-dep list. `reproducible = True`: the repos are a pure function of
    # the module graph and the sha256 table in //toolchain:versions.bzl, so a
    # lockfile copy of them would pin nothing the source does not already.
    hub = ["tofu_toolchains"]
    non_dev = module_ctx.root_module_has_non_dev_dependency
    return module_ctx.extension_metadata(
        root_module_direct_deps = hub if non_dev else [],
        root_module_direct_dev_deps = [] if non_dev else hub,
        reproducible = True,
    )

tofu = module_extension(
    implementation = _tofu_extension_impl,
    tag_classes = {"version": _version_tag},
)
