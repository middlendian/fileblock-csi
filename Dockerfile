FROM golang:1.25-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/middlendian/fileblock-csi/pkg/driver.Version=${VERSION}" \
        -o /out/fileblock-controller ./cmd/controller \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/middlendian/fileblock-csi/pkg/driver.Version=${VERSION}" \
        -o /out/fileblock-node ./cmd/node

# Runtime image needs e2fsprogs (mkfs.ext4, e2fsck, resize2fs), util-linux
# (losetup, mount, umount, findmnt). debian-slim is the smallest image that
# carries all of these without surprises.
FROM debian:trixie-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        e2fsprogs util-linux ca-certificates nfs-common \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/fileblock-controller /usr/local/bin/fileblock-controller
COPY --from=build /out/fileblock-node /usr/local/bin/fileblock-node
ENTRYPOINT ["/usr/local/bin/fileblock-node"]
