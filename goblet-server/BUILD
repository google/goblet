load("@io_bazel_rules_go//go:def.bzl", "go_binary", "go_library")

go_library(
    name = "go_default_library",
    srcs = ["main.go"],
    importpath = "github.com/google/goblet/goblet-server",
    visibility = ["//visibility:private"],
    deps = [
        "//:go_default_library",
        "//google:go_default_library",
        "@com_github_google_uuid//:go_default_library",
        "@com_google_cloud_go//errorreporting:go_default_library",
        "@com_google_cloud_go_logging//:go_default_library",
        "@com_google_cloud_go_storage//:go_default_library",
        "@go_googleapis//google/logging/v2:logging_go_proto",
        "@io_opencensus_go//stats/view:go_default_library",
        "@io_opencensus_go//tag:go_default_library",
        "@io_opencensus_go_contrib_exporter_stackdriver//:go_default_library",
        "@org_golang_x_oauth2//google:go_default_library",
    ],
)

go_binary(
    name = "goblet-server",
    embed = [":go_default_library"],
    visibility = ["//visibility:public"],
)
