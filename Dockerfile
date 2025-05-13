###########                                                                                              # Builder
FROM docker.io/golang:1.23-alpine AS builder

WORKDIR /build

COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY main.go main.go
COPY internal/ internal/
COPY artifacts/ artifacts/

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -trimpath -o sdwan-operator .


###########
# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot

WORKDIR /
COPY --from=builder /build/sdwan-operator .
USER 65532:65532

ENTRYPOINT ["/sdwan-operator"]
