# syntax=docker/dockerfile:1
FROM gcr.io/distroless/static-debian13:nonroot
WORKDIR /
# The build context holds the linux binary at its root, under the name
# goreleaser gives it. `just oci` stages the same shape in dist/oci.
COPY go-galaxy /go-galaxy
ENTRYPOINT [ "/go-galaxy" ]
