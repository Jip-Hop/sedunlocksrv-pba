FROM ubuntu:20.04

ENV DEBIAN_FRONTEND=noninteractive

RUN apt update && \
apt install -y curl libarchive-tools xorriso cpio rsync golang-go git squashfs-tools udev dosfstools fdisk wget grub2-common

WORKDIR /tmp
COPY --chmod=755 . .

CMD ./build.sh