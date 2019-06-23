load("@io_bazel_rules_go//go:def.bzl", "go_library")

go_library(
    name = "go_default_library",
    srcs = [
        "backup.go",
        "hooks.go",
    ],
    importpath = "github.com/google/goblet/google",
    visibility = ["//visibility:public"],
    deps = [
        "//:go_default_library",
        "@com_google_cloud_go//storage:go_default_library",
        "@org_golang_google_api//iterator:go_default_library",
        "@org_golang_google_api//oauth2/v2:go_default_library",
        "@org_golang_google_api//option:go_default_library",
        "@org_golang_google_grpc//codes:go_default_library",
        "@org_golang_google_grpc//status:go_default_library",
        "@org_golang_x_oauth2//:go_default_library",
    ],
)
