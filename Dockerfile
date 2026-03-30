FROM --platform=amd64 ubuntu:22.04

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && \
  apt-get install -y --no-install-recommends cpio curl dosfstools dropbear fdisk git golang-go grub-efi-amd64-bin \
  grub-efi-ia32-bin grub-pc-bin grub2-common libarchive-tools rsync udev wget xorriso ca-certificates && \
  rm -rf /var/lib/apt/lists/*

WORKDIR /tmp
COPY . .

CMD ./build.sh
