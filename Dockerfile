###########
# The builder runs on the build platform and cross-compiles for the target: Go needs no
# emulation for that, an emulated compiler is an order of magnitude slower.
FROM --platform=$BUILDPLATFORM docker.io/golang:1.27-alpine AS builder
ARG TARGETOS TARGETARCH

WORKDIR /build

COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY main.go main.go
COPY internal/ internal/
#COPY artifacts/ artifacts/

RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags="-s -w" -trimpath -o sdwan-operator .


###########
# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot

WORKDIR /
COPY --from=builder /build/sdwan-operator .
COPY artifacts/ artifacts/

USER 65532:65532

ENTRYPOINT ["/sdwan-operator"]
