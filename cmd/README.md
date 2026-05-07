

./diskimg /Users/galaxy/.local/share/carbon/cloud.debian.org/debian/12/arm64/disk.img --fs cat /boot/vmlinuz-6.1.0-45-cloud-arm64 > vmlinuz

./diskimg /Users/galaxy/.local/share/carbon/cloud.debian.org/debian/12/arm64/disk.img --fs cat /boot/initrd.img-6.1.0-45-cloud-arm64 > initrd


/Users/galaxy/Desktop/diskimg/cmd/vmlinuz
/Users/galaxy/Desktop/diskimg/cmd/initrd


./build/cli/vm-cli run \
  --name=debian \
  --cpu=2 \
  --ram=2GB \
  --image=/Users/galaxy/.local/share/carbon/cloud.debian.org/debian/12/arm64/disk.img \
  --kernel=/Users/galaxy/Desktop/diskimg/cmd/vmlinuz \
  --initrd=/Users/galaxy/Desktop/diskimg/cmd/initrd \
  --cmdline="console=hvc0 earlycon root=/dev/vda1 ro" \
  --serial=1