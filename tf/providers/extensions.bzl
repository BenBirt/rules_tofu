"""Bzlmod module extension that downloads pinned OpenTofu/Terraform provider
plugins and exposes them as `tf_provider` build targets, ready to be
referenced from `tf_library(providers = [...])` and
`tf_deploy(providers = [...])`.

Downstream usage in MODULE.bazel:

    tf_providers = use_extension(
        "@rules_tofu//tf/providers:extensions.bzl",
        "tf_providers",
    )
    tf_providers.provider(
        source = "hashicorp/aws",
        version = "5.70.0",
        sha256 = {
            "linux_amd64":   "...",
            "linux_arm64":   "...",
            "darwin_amd64":  "...",
            "darwin_arm64":  "...",
            "windows_amd64": "...",
        },
    )
    use_repo(tf_providers, "tf_providers_hashicorp_aws")

The repo name is derived deterministically from `source`: the `<ns>/<name>`
pair becomes `tf_providers_<ns>_<name>` (e.g. `hashicorp/aws` →
`tf_providers_hashicorp_aws`).
Callers cannot override this name.

When multiple modules in the dependency graph request the same provider (same
`source`), the extension resolves to a single version:

  1. If the **root module** specifies a version, that version wins.
  2. Otherwise the **highest version** across all requesting modules wins
     (simple integer-component semver comparison).

For the winning version, `sha256` maps are unioned across all modules that
declared that version. If two modules provide conflicting sha256 values for
the same platform at the same version, the extension fails with a clear
error.

//toolchain:extensions.bzl resolves the OpenTofu version differently — the
root arbitrates and disagreeing dependencies are an error, never
highest-wins. The asymmetry is deliberate: a provider is a
per-configuration artifact a library genuinely depends on, which puts it in
the same domain as Bzlmod's own MVS, where taking the highest version is
the norm. The tofu binary is a single global tool registered once for the
whole build, and which version a build runs is the root module's call.

The target `@tf_providers_<ns>_<name>//:provider` then carries
`TfProviderInfo` and is passed to `tf_library`/`tf_deploy`
via their `providers` attribute. The `sha256` recorded on each tag gates the
download, so the provider tree is reproducible and is fetched once per
workspace.

See `examples/aws/` and `examples/gcp/` in-tree for full end-to-end uses.

Currently only providers hosted on `releases.hashicorp.com` (i.e. published by
the `hashicorp/` namespace) are supported. Other registries require resolving
the download URL via `registry.opentofu.org`'s download protocol; that can be
added later behind a `url_template` attribute.
"""

# Default plugin-dir host segment. OpenTofu canonicalizes `source = "<ns>/<name>"`
# in `required_providers` to `registry.opentofu.org/<ns>/<name>` and looks for
# plugins under `<plugin-dir>/registry.opentofu.org/...`. rules_tofu uses OpenTofu,
# so the vendored plugin tree mirrors that layout.
_DEFAULT_HOST = "registry.opentofu.org"

# (os/arch -> exe suffix). Keys must match the `sha256` map's platform keys.
_EXE_SUFFIX = {
    "linux_amd64": "",
    "linux_arm64": "",
    "linux_386": "",
    "darwin_amd64": "",
    "darwin_arm64": "",
    "windows_amd64": ".exe",
    "windows_386": ".exe",
}

def _split_source(source):
    parts = source.split("/")
    if len(parts) != 2 or not parts[0] or not parts[1]:
        fail("provider `source` must be \"<namespace>/<name>\", got {}".format(repr(source)))
    return parts[0], parts[1]

def _split_platform_key(platform_key):
    parts = platform_key.split("_", 1)
    if len(parts) != 2 or not parts[0] or not parts[1]:
        fail("provider platform key must be \"<os>_<arch>\", got {}".format(repr(platform_key)))
    return parts[0], parts[1]

def _repo_name_from_source(source):
    """Derive the canonical Bazel repository name from a provider source.

    `hashicorp/aws` → `tf_providers_hashicorp_aws`.
    """
    return "tf_providers_" + source.replace("/", "_")

def _parse_version(version):
    """Parse a version string into a list of integers for comparison.

    Handles standard semver `major.minor.patch`. Pre-release suffixes are
    not supported and will cause a fail().
    """
    parts = version.split(".")
    result = []
    for part in parts:
        if not part.isdigit():
            fail("Cannot parse version component `{}` in version `{}`; only numeric components are supported".format(part, version))
        result.append(int(part))
    return result

def _version_gt(a, b):
    """Return True if version string `a` is strictly greater than `b`."""
    pa = _parse_version(a)
    pb = _parse_version(b)

    # Pad the shorter list with zeros so [5, 70] and [5, 70, 0] compare equal.
    max_len = max(len(pa), len(pb))
    for _ in range(max_len - len(pa)):
        pa.append(0)
    for _ in range(max_len - len(pb)):
        pb.append(0)

    for i in range(max_len):
        if pa[i] > pb[i]:
            return True
        if pa[i] < pb[i]:
            return False
    return False

def _format_binaries_dict(entries):
    """Format `entries` as a Starlark dict literal for the generated BUILD.

    `entries` is a list of `(binary_path, platform_key)` tuples. The dict
    is keyed by file label (so `label_keyed_string_dict` resolves each to a
    File) and valued by platform key string.
    """
    lines = ["    binaries = {"]
    for binary_path, platform_key in entries:
        lines.append("        \"{}\": \"{}\",".format(binary_path, platform_key))
    lines.append("    },")
    return "\n".join(lines)

def _tf_provider_download_impl(repository_ctx):
    source = repository_ctx.attr.source
    version = repository_ctx.attr.version
    sha256 = repository_ctx.attr.sha256
    namespace, name = _split_source(source)

    if namespace != "hashicorp":
        fail(
            ("tf_providers.provider `{}` has source `{}`: only providers from " +
             "the `hashicorp/` namespace (served via releases.hashicorp.com) are " +
             "currently supported. Support for other registries can be added by " +
             "extending the extension with a `url_template` attribute.").format(
                repository_ctx.attr.name,
                source,
            ),
        )

    if not sha256:
        fail("tf_providers.provider `{}` must declare at least one platform in `sha256`".format(
            repository_ctx.attr.name,
        ))

    address = "{}/{}/{}".format(_DEFAULT_HOST, namespace, name)
    entries = []
    for platform_key in sorted(sha256.keys()):
        if platform_key not in _EXE_SUFFIX:
            fail("tf_providers.provider `{}` declares unknown platform `{}`. Known: {}".format(
                repository_ctx.attr.name,
                platform_key,
                sorted(_EXE_SUFFIX.keys()),
            ))
        os_name, arch = _split_platform_key(platform_key)
        extract_dir = "plugins/{host}/{ns}/{name}/{version}/{platform}".format(
            host = _DEFAULT_HOST,
            ns = namespace,
            name = name,
            version = version,
            platform = platform_key,
        )
        url = ("https://releases.hashicorp.com/terraform-provider-{name}/{version}/" +
               "terraform-provider-{name}_{version}_{os}_{arch}.zip").format(
            name = name,
            version = version,
            os = os_name,
            arch = arch,
        )

        repository_ctx.download_and_extract(
            url = url,
            sha256 = sha256[platform_key],
            type = "zip",
            output = extract_dir,
        )

        # The zip contains the provider binary (typically
        # `terraform-provider-<name>_v<version>{_x5}{.exe}`) plus, on some
        # releases, a LICENSE file. Pick the file whose name starts with the
        # canonical prefix so we don't accidentally grab the LICENSE.
        expected_prefix = "terraform-provider-{}_v{}".format(name, version)
        exe_suffix = _EXE_SUFFIX[platform_key]
        binary_filename = None
        for child in repository_ctx.path(extract_dir).readdir():
            basename = child.basename
            if basename.startswith(expected_prefix) and basename.endswith(exe_suffix):
                if binary_filename != None:
                    fail(("Ambiguous provider binary for {}@{} on {}: both `{}` and " +
                          "`{}` match `{}*{}`. Refusing to guess.").format(
                        source,
                        version,
                        platform_key,
                        binary_filename,
                        basename,
                        expected_prefix,
                        exe_suffix,
                    ))
                binary_filename = basename
        if binary_filename == None:
            fail("No file matching `{}*{}` found in {} after extracting {}".format(
                expected_prefix,
                exe_suffix,
                extract_dir,
                url,
            ))

        entries.append(("{}/{}".format(extract_dir, binary_filename), platform_key))

    # Extracted binaries live in nested directories; Bazel doesn't auto-expose
    # them as source labels, so the `label_keyed_string_dict` on
    # `tf_provider` couldn't resolve them without an explicit
    # `exports_files()`.
    exports_lines = ["exports_files(["]
    for binary_path, _ in entries:
        exports_lines.append("    \"{}\",".format(binary_path))
    exports_lines.append("])")

    repository_ctx.file("WORKSPACE", "")
    repository_ctx.file(
        "BUILD.bazel",
        content = """\
load("@rules_tofu//tf/private:provider.bzl", "tf_provider")

package(default_visibility = ["//visibility:public"])

{exports}

tf_provider(
    name = "provider",
    address = "{address}",
    version = "{version}",
{binaries}
)
""".format(
            exports = "\n".join(exports_lines),
            address = address,
            version = version,
            binaries = _format_binaries_dict(entries),
        ),
        executable = False,
    )

_tf_provider_download = repository_rule(
    implementation = _tf_provider_download_impl,
    attrs = {
        "source": attr.string(mandatory = True),
        "version": attr.string(mandatory = True),
        "sha256": attr.string_dict(mandatory = True),
    },
)

_provider_tag = tag_class(
    attrs = {
        "source": attr.string(
            mandatory = True,
            doc = "Provider source in `<namespace>/<name>` form, e.g. \"hashicorp/aws\".",
        ),
        "version": attr.string(
            mandatory = True,
            doc = "Provider version to pin, e.g. \"5.70.0\".",
        ),
        "sha256": attr.string_dict(
            mandatory = True,
            doc = "Per-platform SHA256 of the provider zip. Keys are `<os>_<arch>` " +
                  "(e.g. `linux_amd64`). Declare every platform you intend to run on; " +
                  "build/runtime fail at deploy time if the exec platform is missing.",
        ),
    },
)

def _tf_providers_extension_impl(module_ctx):
    # Phase 1: Collect all provider tags into a flat list of structs,
    # then group by source.
    all_tags = []

    # Repos the root is expected to `use_repo`: only those backing its own
    # tags. A repo that exists purely because a dependency asked for the
    # provider is not a direct dep of the root.
    root_repos = []
    for mod in module_ctx.modules:
        for tag in mod.tags.provider:
            all_tags.append(struct(
                source = tag.source,
                version = tag.version,
                sha256 = tag.sha256,
                is_root = mod.is_root,
                mod_name = mod.name,
            ))
            repo_name = _repo_name_from_source(tag.source)
            if mod.is_root and repo_name not in root_repos:
                root_repos.append(repo_name)

    by_source = {}
    for t in all_tags:
        if t.source not in by_source:
            by_source[t.source] = []
        by_source[t.source].append(t)

    # Phase 2: Resolve each provider to a single (version, sha256) pair.
    for source, tags in by_source.items():
        # Validate: at most one root declaration per provider.
        root_tags = [t for t in tags if t.is_root]
        if len(root_tags) > 1:
            fail("Root module declares provider `{}` more than once".format(source))

        # Determine the winning version.
        if root_tags:
            # Root module wins unconditionally.
            resolved_version = root_tags[0].version
        else:
            # Pick the highest version across all tags.
            resolved_version = tags[0].version
            for t in tags[1:]:
                if _version_gt(t.version, resolved_version):
                    resolved_version = t.version

        # Merge sha256 maps from all tags that declared the winning version.
        merged_sha256 = {}
        for t in tags:
            if t.version != resolved_version:
                continue
            for platform, digest in t.sha256.items():
                if platform in merged_sha256 and merged_sha256[platform] != digest:
                    fail(
                        ("Provider `{}` version `{}`: modules disagree on sha256 for " +
                         "platform `{}` (got `{}` and `{}`)").format(
                            source,
                            resolved_version,
                            platform,
                            merged_sha256[platform],
                            digest,
                        ),
                    )
                merged_sha256[platform] = digest

        # Phase 3: Create the repository.
        repo_name = _repo_name_from_source(source)
        _tf_provider_download(
            name = repo_name,
            source = source,
            version = resolved_version,
            sha256 = merged_sha256,
        )

    # `reproducible = True`: every download is gated by the `sha256` attribute
    # on the tag that requested it, so the repos are a pure function of the
    # module graph and a lockfile copy would pin nothing MODULE.bazel does not.
    non_dev = module_ctx.root_module_has_non_dev_dependency
    return module_ctx.extension_metadata(
        root_module_direct_deps = root_repos if non_dev else [],
        root_module_direct_dev_deps = [] if non_dev else root_repos,
        reproducible = True,
    )

tf_providers = module_extension(
    implementation = _tf_providers_extension_impl,
    tag_classes = {"provider": _provider_tag},
)
