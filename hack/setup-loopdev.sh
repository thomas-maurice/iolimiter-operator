#!/bin/bash
# setup-loopdev.sh
#
# Runs INSIDE the kind worker node (via `docker exec <cluster>-worker`).
# Creates two loopback block devices the e2e suite throttles via io.max:
# 512Mi sparse files -> losetup -> mkfs.ext4 -> mount under
# /mnt/blkio-e2e/disk{0,1}. Idempotent: every step is skipped if its result
# already exists, so `make kind-setup` works from scratch and re-running it
# against an already-provisioned worker is a no-op that just reprints the
# MAJ:MIN of each device.
#
# Usage:
#   docker exec <cluster>-worker bash /hack/setup-loopdev.sh

set -euo pipefail

IMG_DIR="/var/lib/blkio-e2e"
MNT_DIR="/mnt/blkio-e2e"
SIZE_MB=512
DISKS="disk0 disk1"

mkdir -p "${IMG_DIR}" "${MNT_DIR}"

for disk in ${DISKS}; do
	img="${IMG_DIR}/${disk}.img"
	mnt="${MNT_DIR}/${disk}"
	mkdir -p "${mnt}"

	if mountpoint -q "${mnt}"; then
		loop_dev=$(findmnt -no SOURCE "${mnt}")
		maj_min=$(lsblk -ndo MAJ:MIN "${loop_dev}")
		echo "=== ${disk}: already mounted at ${mnt} (${loop_dev}, ${maj_min}) ==="
		continue
	fi

	if [ ! -f "${img}" ]; then
		echo "=== ${disk}: creating ${SIZE_MB}Mi sparse image at ${img} ==="
		truncate -s "${SIZE_MB}M" "${img}"
	fi

	loop_dev=$(losetup -j "${img}" | cut -d: -f1)
	if [ -z "${loop_dev}" ]; then
		echo "=== ${disk}: attaching loop device ==="
		loop_dev=$(losetup --find --show "${img}")
	fi
	echo "${disk}: loop device ${loop_dev}"

	if ! blkid "${loop_dev}" >/dev/null 2>&1; then
		echo "=== ${disk}: formatting ${loop_dev} as ext4 ==="
		mkfs.ext4 -q "${loop_dev}"
	fi

	echo "=== ${disk}: mounting ${loop_dev} at ${mnt} ==="
	mount "${loop_dev}" "${mnt}"

	maj_min=$(lsblk -ndo MAJ:MIN "${loop_dev}")
	echo "=== ${disk}: done: mount=${mnt} device=${loop_dev} maj:min=${maj_min} ==="
done

echo "=== loop device summary ==="
for disk in ${DISKS}; do
	mnt="${MNT_DIR}/${disk}"
	loop_dev=$(findmnt -no SOURCE "${mnt}")
	maj_min=$(lsblk -ndo MAJ:MIN "${loop_dev}")
	echo "${disk}: mount=${mnt} device=${loop_dev} maj:min=${maj_min}"
done
