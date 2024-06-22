# Copyright 2024 The Bazel Authors. All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""A module extension for downloading uv."""

load("//python/private/uv:uv_hub.bzl", "uv_hub")

def _impl(module_ctx):
    repos = []
    dev_repos = []
    for mod in module_ctx.modules:
        for tag in mod.tags.download:
            print_warning = False
            want_files = {}
            urls = {}
            for file, want_sha256 in tag.files.items():
                url = tag.url_template.format(
                    version = tag.version,
                    file = file,
                )
                if not want_sha256:
                    print_warning = True
                    sha256_file = module_ctx.path(file + ".sha256")
                    module_ctx.download(
                        url = url + ".sha256",
                        output = sha256_file,
                    )
                    want_sha256 = module_ctx.read(sha256_file).split(" ")[0]

                want_files[file] = want_sha256
                urls[file] = url

            if print_warning:
                # buildifier: disable=print
                print("\n".join(
                    [
                        "WARNING: Update the uv.download 'files' attribute to be:",
                        " ".join([
                            "buildozer",
                            "'remove files'",
                        ] + [
                            "'dict_set files {}:{}'".format(k, v)
                            for k, v in want_files.items()
                        ] + [
                            "MODULE.bazel:%uv.download",
                        ]),
                    ],
                ))

            uv_repos = uv_hub(
                name = tag.hub_name,
                filenames = want_files,
                urls = urls,
            )
            if module_ctx.is_dev_dependency(tag):
                dev_repos.extend(uv_repos)
            else:
                repos.extend(uv_repos)

    return module_ctx.extension_metadata(
        root_module_direct_deps = repos,
        root_module_direct_dev_deps = dev_repos,
    )

uv = module_extension(
    implementation = _impl,
    tag_classes = {
        "download": tag_class(
            attrs = {
                "files": attr.string_dict(
                    mandatory = True,
                ),
                "hub_name": attr.string(
                    default = "rules_python_uv",
                ),
                "url_template": attr.string(
                    default = "https://github.com/astral-sh/uv/releases/download/{version}/{file}",
                ),
                "version": attr.string(mandatory = True),
            },
        ),
    },
    doc = """A module extension for downloading uv.""",
)