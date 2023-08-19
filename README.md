# sedunlocksrv-pba
Conveniently unlock your Self Encrypting Drive on startup (via HTTPS or SSH) without the need to attach monitor and keyboard.

<img width="450" alt="screenshot" src="https://user-images.githubusercontent.com/2871973/118612963-91a34e00-b7be-11eb-9480-9d23e427982c.png">

## Disclaimer
Use at your own risk! You may lock yourself out of the data on the disk.

## Compatibility
This tool, `sedunlocksrv-pba`, will only work if you have a Self Encrypting Drive (SED) which is compatible with [sedutil](https://github.com/Drive-Trust-Alliance/sedutil) (TCG OPAL). For example the Samsung EVO 850 SSD.

## Use case
Fully encrypt your home server or NAS and conveniently unlock it on startup without the need to attach monitor and keyboard. Unlocking can be done from any device on your LAN with a browser. By default a self-signed HTTPS certificate is used (generated during building) to secure the unlocking.

Because the drive is using hardware encryption, you can encrypt your server if the OS doesn't support encryption at all, or only for some disks (e.g. no encryption for the drive on which the OS is installed).

Even for systems which support encrypting all drives, using a SED with `sedunlocksrv-pba` can be useful because of the remote unlock functionality. Unlock and continue booting from any device on your LAN via HTTPS/SSH. If you're using a password manager you can conveniently auto-fill the unlock password.

## Features
- Unlock your SED from a browser (via HTTPS)
- Unlock your SED via SSH
- Change disk password from a browser (via HTTPS)
- Not limited to us_english keyboard mapping
- Reboot button to boot from the unlocked drive
- BIOS and UEFI support

## SED benefits
- Encrypt your (boot) drive, even when the OS doesn't (fully) support encryption
- Drive locks when power is lost, protecting data when server is stolen 
- Hardware encryption means less CPU usage 

## Requirements
- A Self Encrypting Drive compatible with [sedutil](https://github.com/Drive-Trust-Alliance/sedutil) (TCG OPAL)
- Ubuntu to build the PBA image
- Two USB sticks to flash the PBA image

## Building with Docker

This allows building the image with Docker, even on ARM architecture (Apple Silicon / M1 processor).

```bash
(NAME=sedunlocksrv-pba; docker build -t $NAME . && docker run --name $NAME --privileged $NAME && docker cp $NAME:/tmp/sedunlocksrv-pba.img sedunlocksrv-pba.img; docker rm $NAME)
```

After running the command above you will find sedunlocksrv-pba.img in your current working directory. Continue with [Encrypting your drive and flashing the PBA](#encrypting-your-drive-and-flashing-the-pba).

## Setup a VM for building with VirtualBox
- Download and install [VirtualBox](https://www.virtualbox.org/wiki/Downloads)
- Also install the VirtualBox Extension Pack from the link above
- [Download Ubuntu 20.04.2 Focal Fossa](https://sourceforge.net/projects/linuxvmimages/files/VirtualBox/U/20.04/Ubuntu_20.04.2_VB.zip/download) from [linuxvmimages](https://www.linuxvmimages.com/images/ubuntu-2004)
- Extract the downloaded archive
- Import the VM by double clicking the `Ubuntu_20.04.2_VB_LinuxVMImages.COM.ova` file
- Open Settings for the newly created VM and go to Ports->USB to enable the USB 3.0 (xHCI) Controller
- Boot the VM and login with username `ubuntu` and password `ubuntu`
- Tip: enable Shared Clipboard from the Devices dropdown menu to copy and paste the commands in the next steps
- Optional: open Terminal and run `sudo apt-get -y install nautilus-admin && sudo adduser $USER vboxsf` for convenience (access VirtualBox shared folders and browse in Files as admin via right click -> Open as Administrator)
- Insert the `Guest Additions CD image` from the `Devices` menu dropdown, update the installation and reboot
- Open Terminal and become root with: `sudo su`
- Update with: `apt-get update && apt-get -y upgrade`
- Continue with building in the next steps

## Building on Ubuntu 20.04 LTS or Ubuntu 22.04 LTS
- Install the Go compiler with: `snap install go --classic`
- Install build dependencies: `apt-get -y install curl libarchive-tools grub-pc-bin grub-efi-ia32-bin grub-efi-amd64-bin xorriso wget git cpio rsync squashfs-tools udev dosfstools fdisk grub2-common`
- [Download](https://github.com/Jip-Hop/sedunlocksrv-pba/archive/refs/heads/main.zip) or clone this repo and run: `./build.sh`
- Connect your USB stick to Ubuntu (if inside VirtualBox, use the Devices dropdown menu)
- Format the stick with a supported filesystem (e.g. FAT32) if this is not already the case
- Copy the `sedunlocksrv-pba.img` file onto your USB stick (use the GUI file explorer or `cp` from the Terminal)
- Eject the USB stick and put it aside for now
- Use the other USB stick for the sedutil rescue system (see next step)

## Optional SED unlock via SSH

<img width="490" alt="screenshot" src="https://user-images.githubusercontent.com/15635386/235292505-39fd4461-ea31-4ee3-b98e-df76aa311b94.png">

Optionally SED disks can be unlocked via SSH. To enable this feature (in addition to HTTPS unlocking) follow above build steps with small extras:

- install dropbear (it will be used to generate dropbear host keys):`apt-get -y install dropbear`
- create authorized_keys file in `sedunlocksrv-pba/ssh` folder. It should contain public keys of all key pairs allowed to connect to unlocking service. Have a look at provided `sedunlocksrv-pba/ssh/authorized_keys.example`
- run build with SSH option: `./build.sh SSH`

Usage:
run `ssh -p 2222 tc@IP` --> enter SED disk password --> repeat for other disks (if all disks have the same password they will be unlocked in one step) --> press ESC to reboot.

It uses port `2222` to avoid certificates' conflicts with booted computer and `tc` default Tiny Core Linux user. It only allows to access SED unlocking with any other SSH services disabled.

## Excluding network device(s)

Note that by default, the PBA image will try to configure all network devices with dynamic IP addresses using DHCP, and the web server and SSH server will listen on all interfaces. That may not be desirable in some cases (e.g. if some network device(s) is/are exposed to the Internet).

To solve this problem, optionally it is possible to set a list of network devices to be excluded when running the build script, for example:
```
sudo EXCLUDE_NETDEV="eth0 eth1" ./build.sh
```
will exclude `eth0` and `eth1` from DHCP configuration.

## Encrypting your drive and flashing the PBA
Follow [the instructions](https://github.com/Drive-Trust-Alliance/sedutil/wiki/Encrypting-your-drive) from the official Drive Trust Alliance sedutil wiki page. Except when you arrive at step `Enable locking and the PBA`, don't `gunzip` and flash the included `/usr/sedutil/UEFI64-n.nn.img` file. This is where you connect the USB stick with the `sedunlocksrv-pba.img`. Check the output of `fdisk -l` to see to which device this USB stick is mapped. In my case it's `/dev/sdg1`. Mount the USB with `mount /dev/sdg1 /mnt/`. Now flash the custom PBA with `sedutil-cli --loadpbaimage debug /mnt/sedunlocksrv-pba.img /dev/sdc`. Make sure to replace `/dev/sdc` so it targets your SED. Additionally I recommend that you set a simple password when arriving at the `Set a real password` step. For example use `test`. Set your real password through the web interface when booting from sedunlocksrv-pba.

## Tips
- Flash the PBA to all the Self Encrypting Drives in your server
- Use the same password for all the SEDs in your server (otherwise you need to enter multiple passwords during startup)
- Replace the `server.crt` and `server.key` (found inside the sedunlocksrv after running `./build.sh`) if you like, or modify `make-cert.sh` and run `./build.sh` again

## Wishlist
- Faster booting after unlock, similar to [opal-kexec-pba](https://github.com/jnohlgard/opal-kexec-pba)
- PBA flashing via the web interface

## References
- [Into the Core](http://www.tinycorelinux.net/corebook.pdf) to understand the Tiny Core Linux boot process
- Build script based on [custom-tinycore.sh](https://gist.github.com/dankrause/2a9ed5ed30fa7f9aaaa2)
- SED unlock code borrowed from [opal-functions.sh](https://github.com/rear/rear/blob/6a3d0b4d5e73c69a62ce0bd209b2b38ffb462569/usr/share/rear/lib/opal-functions.sh) and [unlock-opal-disks](https://github.com/rear/rear/blob/6a3d0b4d5e73c69a62ce0bd209b2b38ffb462569/usr/share/rear/skel/default/etc/scripts/unlock-opal-disks)
- [Example to handle GET and POST request in Golang](https://www.golangprograms.com/example-to-handle-get-and-post-request-in-golang.html)
- [How to redirect HTTP to HTTPS with a golang webserver](https://gist.github.com/d-schmidt/587ceec34ce1334a5e60)
- [How do I get the local IP address in Go?](https://stackoverflow.com/a/37382208/)
- [Simple login form example](https://www.w3schools.com/howto/howto_css_login_form.asp)
- [Fix to get the 64-bit binaries working](http://forum.tinycorelinux.net/index.php?topic=19607.0)
- Guides on installing GRUB: [grub2-bios-uefi-usb](https://github.com/ndeineko/grub2-bios-uefi-usb) and [grub_hybrid](https://www.normalesup.org/~george/comp/live_iso_usb/grub_hybrid.html)
