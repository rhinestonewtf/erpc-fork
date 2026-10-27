# syntax = docker/dockerfile:1.4

# This Dockerfile is used to build the eRPC server image.
# Docker build stages:
#     - go-builder -> build the go binary
#     - ts-core -> Core stage for TS related stuff (just installing pnpm)
#     - ts-dev -> Install dev dependencies and compile the SDK
#     - ts-prod -> Install prod dependencies only
#     - final -> Final stage where we copy the Go binary and the TS files

# Build stage for Go
# The toolchain must be go1.26.10+ (or go1.27.3+): those releases add the
# crypto/x509 distrust list for the compromised eMudhra intermediates
# (golang/go#82345). The list is compiled into the binary, so the runtime
# image's CA bundle cannot replace it. scripts/check-go-toolchain.sh enforces
# this below; keep the tag and the digest on the same release.
FROM golang:1.26.10-alpine@sha256:a2012b4a1d1f9f7e9f44e0d39b805a901e856b1dd80b4e6ee4338db547050308 AS go-builder

WORKDIR /build

# Fail fast on a downgraded base image, before downloading modules. The guard's
# own tests run first, so a broken guard cannot pass every version.
COPY scripts/check-go-toolchain.sh scripts/check-go-toolchain_test.sh /build/scripts/
RUN sh scripts/check-go-toolchain_test.sh scripts/check-go-toolchain.sh && \
    sh scripts/check-go-toolchain.sh "$(go env GOVERSION)"

# Copy go mod and sum files first for better layer caching
COPY go.mod go.sum ./
RUN go mod download

# Copy the source code
COPY . .

# Set build arguments
ARG VERSION
ARG COMMIT_SHA

# Set environment variables for Go build
ENV CGO_ENABLED=0 \
    GOOS=linux \
    LDFLAGS="-w -s -X github.com/erpc/erpc/common.ErpcVersion=${VERSION} -X github.com/erpc/erpc/common.ErpcCommitSha=${COMMIT_SHA}"

# Build the Go binary
RUN go build -v -ldflags="$LDFLAGS" -a -installsuffix cgo -o erpc-server ./cmd/erpc/main.go && \
    go build -v -ldflags="$LDFLAGS" -a -installsuffix cgo -tags pprof -o erpc-server-pprof ./cmd/erpc/*.go

# Check the toolchain recorded in the binaries themselves. GOTOOLCHAIN could
# switch to a different toolchain than the image's, and this is what would see it.
RUN sh scripts/check-go-toolchain.sh --binary erpc-server erpc-server-pprof

# Global typescript related image
# Node runs only at build time: the final image receives the compiled
# @erpc-cloud/config package and its prod node_modules, never the node binary.
FROM node:24.22.1-alpine@sha256:06d7adf3e0ef61a13f1d28682726366f97a15aa1888c934d3ecec86c33918b53 AS ts-core
# Pin to package.json `packageManager`: an unpinned install pulls the newest
# pnpm, which re-execs itself to the pinned version and fails on alpine with
# ERR_PNPM_PNPM_ENGINE_NO_NATIVE_BINARY (main release builds, 2026-09-29/30).
RUN npm install -g pnpm@10.28.2

# Stage where we will install dev dependencies + compile sdk
FROM ts-core AS ts-dev
RUN mkdir -p /temp/dev/typescript

# Copy only the TypeScript package files
COPY typescript/config /temp/dev/typescript/config
COPY pnpm* /temp/dev/
COPY package.json /temp/dev/package.json

# Install everything and build
RUN --mount=type=cache,id=pnpm,target=/pnpm/store cd /temp/dev &&  pnpm install --frozen-lockfile
RUN cd /temp/dev && pnpm build

# Stage where we will install prod dependencies only
FROM ts-core AS ts-prod
RUN mkdir -p /temp/prod/typescript

COPY typescript/config /temp/prod/typescript/config
COPY pnpm* /temp/prod/
COPY package.json /temp/prod/package.json

# Install every prod dependencies
RUN --mount=type=cache,id=pnpm,target=/pnpm/store cd /temp/prod && pnpm install --prod --frozen-lockfile

# Create symlink stage (for backwards compatibility with earlier image file structure)
FROM alpine:3.24.3@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a AS symlink
RUN mkdir -p /root && ln -s /erpc-server /root/erpc-server

# Final stage
# Its Debian CA bundle still contains the emSign and Sectigo AAA roots that
# chain to the compromised eMudhra intermediates. Protection comes from the Go
# toolchain check in go-builder, not from this bundle.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS final

# Copy Go binaries from go-builder
COPY --from=go-builder /build/erpc-server /
COPY --from=go-builder /build/erpc-server-pprof /

# Copy symlinked directory with preserved symlinks
COPY --from=symlink --link /root /root

# Copy TypeScript package files from ts-dev and ts-prod
COPY --from=ts-dev /temp/dev/typescript /typescript
COPY --from=ts-prod /temp/prod/node_modules /node_modules

# Expose ports
EXPOSE 4000 4001 6060

# Run the server
CMD ["/erpc-server"]
