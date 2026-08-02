# This Dockerfile does NOT build crashcause from source. goreleaser builds
# the `crashcause` binary for the target platform and passes it into this
# build's context (see .goreleaser.yaml `dockers:`), so all this does is
# place the prebuilt binary into a minimal, non-root distroless image.
#
# OCI labels (source/version/created/revision/licenses) are applied at build
# time via .goreleaser.yaml's `build_flag_templates` rather than hardcoded
# here, so there is one owner for them and no stale/duplicate values.
FROM gcr.io/distroless/static:nonroot

COPY crashcause /crashcause

USER nonroot:nonroot

ENTRYPOINT ["/crashcause"]
