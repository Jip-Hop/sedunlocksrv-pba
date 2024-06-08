FROM --platform=amd64 ubuntu:22.04

ENV DEBIAN_FRONTEND=noninteractive

RUN apt update && \
  apt install -y cpio curl dosfstools dropbear fdisk git golang-go grub-efi-amd64-bin \
  grub-efi-ia32-bin grub-pc-bin grub2-common libarchive-tools rsync udev wget xorriso

WORKDIR /tmp
COPY . .

CMD ./build.sh
