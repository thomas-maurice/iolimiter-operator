#!/bin/bash
# setup-loopdev.sh
#
# Run this inside the kind node (via docker exec) to create a loopback
# block device that io.max can throttle.
#
# Usage:
#   docker exec k8s-blkio-limiter-test-control-plane bash /scripts/setup-loopdev.sh

set -euo pipefail

LOOP_FILE="/tmp/io-test-disk.img"
MOUNT_POINT="/mnt/test-block-vol"
SIZE="64M"

echo "=== Creating ${SIZE} disk image at ${LOOP_FILE} ==="
dd if=/dev/zero of="${LOOP_FILE}" bs=1M count=64 status=progress

echo "=== Setting up loop device ==="
LOOP_DEV=$(losetup --find --show "${LOOP_FILE}")
echo "Loop device: ${LOOP_DEV}"

echo "=== Formatting as ext4 ==="
mkfs.ext4 -q "${LOOP_DEV}"

echo "=== Mounting at ${MOUNT_POINT} ==="
mkdir -p "${MOUNT_POINT}"
mount "${LOOP_DEV}" "${MOUNT_POINT}"

# Show the device info (this is what io.max will target)
MAJ_MIN=$(lsblk -ndo MAJ:MIN "${LOOP_DEV}")
echo ""
echo "=== Done ==="
echo "Mount point:  ${MOUNT_POINT}"
echo "Loop device:  ${LOOP_DEV}"
echo "Major:Minor:  ${MAJ_MIN}"
echo ""
echo "The test pod should mount hostPath: ${MOUNT_POINT}"
echo "and set annotation: blkio-limiter.maurice.fr/<name>: \"path=${MOUNT_POINT} riops=100 wiops=50\""
echo ""
echo "Verify with: findmnt ${MOUNT_POINT}"
findmnt "${MOUNT_POINT}"
