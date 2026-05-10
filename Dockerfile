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
# (losetup, mount, umount, findmnt), and nfs-common (mount.nfs).
#
# Pinned to debian:bookworm-slim (Debian 12) rather than trixie-slim
# (Debian 13). Bookworm ships nfs-utils 2.6.2; trixie ships 2.8.3.
# nfs-utils 2.8.x has a NFSv3 mount regression that surfaces as
# "Protocol not supported" against some servers (verbose trace shows
# mount.nfs successfully discovers NFS port 2049/TCP and mountd
# port/UDP, then fails). csi-driver-nfs's published image uses
# nfs-utils 2.6.2 and works against the same NAS, so match that
# version. Revisit when 2.8.x stabilizes or when we have evidence
# that a newer release fixes the regression.
FROM debian:bookworm-slim
# netbase provides /etc/services, /etc/protocols, /etc/rpc — the
# name<->number tables mount.nfs uses during NFSv3 protocol
# negotiation. Without it the kernel mount(2) call returns
# EPROTONOSUPPORT, which mount.nfs surfaces as "Protocol not
# supported" even though portmapper discovery already succeeded.
# debian:bookworm-slim strips netbase; nfs-common doesn't depend on
# it. csi-driver-nfs's Dockerfile installs netbase explicitly for
# the same reason.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        e2fsprogs util-linux ca-certificates nfs-common netbase \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/fileblock-controller /usr/local/bin/fileblock-controller
COPY --from=build /out/fileblock-node /usr/local/bin/fileblock-node
ENTRYPOINT ["/usr/local/bin/fileblock-node"]
