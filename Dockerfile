FROM --platform=amd64 ubuntu:22.04

ENV DEBIAN_FRONTEND=noninteractive

# Install all build dependencies required by build.sh
# See: check_host_dependencies() in build.sh for the complete list
RUN apt-get update && \
  apt-get install -y --no-install-recommends \
    build-essential \
    bsdtar \
    ca-certificates \
    coreutils \
    cpio \
    curl \
    dosfstools \
    dropbear-bin \
    fdisk \
    findutils \
    git \
    golang-go \
    gzip \
    grub-common \
    grub-efi-amd64-bin \
    grub-efi-ia32-bin \
    grub-pc-bin \
    openssl \
    rsync \
    udev \
    util-linux \
    wget \
    xorriso \
    xz-utils && \
  rm -rf /var/lib/apt/lists/*

WORKDIR /tmp
COPY . .

CMD ./build.sh
