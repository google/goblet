load("@bazel_tools//tools/build_defs/repo:git.bzl", "git_repository")
load("@bazel_tools//tools/build_defs/repo:http.bzl", "http_archive")

http_archive(
    name = "io_bazel_rules_go",
    sha256 = "313f2c7a23fecc33023563f082f381a32b9b7254f727a7dd2d6380ccc6dfe09b",
    urls = [
        "https://storage.googleapis.com/bazel-mirror/github.com/bazelbuild/rules_go/releases/download/0.19.3/rules_go-0.19.3.tar.gz",
        "https://github.com/bazelbuild/rules_go/releases/download/0.19.3/rules_go-0.19.3.tar.gz",
    ],
)

load("@io_bazel_rules_go//go:deps.bzl", "go_rules_dependencies", "go_register_toolchains")

go_rules_dependencies()

go_register_toolchains()

git_repository(
    name = "bazel_gazelle",
    commit = "a8ad29a27bed277667619c927c1783f29681d564",
    remote = "https://github.com/bazelbuild/bazel-gazelle",
    shallow_since = "1564676757 -0400",
)

load("@bazel_gazelle//:deps.bzl", "gazelle_dependencies", "go_repository")

gazelle_dependencies()

git_repository(
    name = "com_google_protobuf",
    commit = "655310ca192a6e3a050e0ca0b7084a2968072260",
    remote = "https://github.com/protocolbuffers/protobuf",
    shallow_since = "1565024848 -0700",
)

load("@com_google_protobuf//:protobuf_deps.bzl", "protobuf_deps")

protobuf_deps()

load("//:goblet_deps.bzl", "goblet_deps")

goblet_deps()
