load("@io_bazel_rules_go//go:def.bzl", "go_library")
load("@bazel_gazelle//:def.bzl", "gazelle")

# gazelle:prefix github.com/google/goblet
# gazelle:build_file_name BUILD
gazelle(name = "gazelle")

go_library(
    name = "go_default_library",
    srcs = [
        "git_protocol_v2_handler.go",
        "goblet.go",
        "http_proxy_server.go",
        "io.go",
        "managed_repository.go",
        "reporting.go",
    ],
    importpath = "github.com/google/goblet",
    visibility = ["//visibility:public"],
    deps = [
        "@com_github_go_git_go_git_v5//:go_default_library",
        "@com_github_go_git_go_git_v5//plumbing:go_default_library",
        "@com_github_google_gitprotocolio//:go_default_library",
        "@com_github_grpc_ecosystem_grpc_gateway//runtime:go_default_library",
        "@io_opencensus_go//stats:go_default_library",
        "@io_opencensus_go//tag:go_default_library",
        "@org_golang_google_grpc//codes:go_default_library",
        "@org_golang_google_grpc//status:go_default_library",
        "@org_golang_x_oauth2//:go_default_library",
    ],
)
