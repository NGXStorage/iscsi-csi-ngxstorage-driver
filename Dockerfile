ARG UBI_IMAGE=registry.access.redhat.com/ubi9/ubi-minimal@sha256:7fbeae18dc9476399f565e68255f602a3374ea8614ba3d14843565131a13ff93
FROM ${UBI_IMAGE}

ARG VERSION=dev
ARG RELEASE=dev
ARG VCS_REF=unknown

LABEL name="NGX Storage iSCSI CSI Driver" \
      maintainer="NGX Storage" \
      vendor="NGX Storage" \
      version="${VERSION}" \
      release="${RELEASE}" \
      summary="NGX Storage iSCSI CSI Driver" \
      description="Container Storage Interface driver for NGX Storage iSCSI block volumes" \
      org.opencontainers.image.title="NGX Storage iSCSI CSI Driver" \
      org.opencontainers.image.vendor="NGX Storage" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${VCS_REF}" \
      org.opencontainers.image.source="https://github.com/NGXStorage/iscsi-csi-ngxstorage-driver" \
      vcs-ref="${VCS_REF}" \
      vcs-type="git"

# The node operand runs host tools through the /host chroot wrapper (see
# cmd/chroot-host-wrapper.sh): iscsiadm, multipath, mkfs, mount and friends are
# invoked on the host. Keep the runtime image minimal; util-linux is still
# required for chroot.
RUN microdnf install -y \
      bash \
      util-linux \
      ca-certificates \
    && microdnf clean all \
    && mkdir -p /chroot /licenses

COPY cmd/chroot-host-wrapper.sh /chroot/chroot-host-wrapper.sh
COPY cmd/entrypoint.sh /entrypoint.sh
COPY cmd/csi-driver/iscsi-csi-ngxstorage-driver /usr/local/bin/iscsi-csi-ngxstorage-driver
COPY LICENSE /licenses/LICENSE

RUN chmod +x /usr/local/bin/iscsi-csi-ngxstorage-driver \
    && chmod +x /entrypoint.sh \
    && chmod +x /chroot/chroot-host-wrapper.sh \
    && ln -s /chroot/chroot-host-wrapper.sh /chroot/iscsiadm \
    && ln -s /chroot/chroot-host-wrapper.sh /chroot/blockdev \
    && ln -s /chroot/chroot-host-wrapper.sh /chroot/scsi_id \
    && ln -s /chroot/chroot-host-wrapper.sh /chroot/lsscsi \
    && ln -s /chroot/chroot-host-wrapper.sh /chroot/rescan-scsi-bus.sh \
    && ln -s /chroot/chroot-host-wrapper.sh /chroot/multipath \
    && ln -s /chroot/chroot-host-wrapper.sh /chroot/multipathd \
    && ln -s /chroot/chroot-host-wrapper.sh /chroot/dmsetup \
    && ln -s /chroot/chroot-host-wrapper.sh /chroot/pgrep \
    && ln -s /chroot/chroot-host-wrapper.sh /chroot/mount \
    && ln -s /chroot/chroot-host-wrapper.sh /chroot/umount \
    && ln -s /chroot/chroot-host-wrapper.sh /chroot/findmnt \
    && ln -s /chroot/chroot-host-wrapper.sh /chroot/blkid \
    && ln -s /chroot/chroot-host-wrapper.sh /chroot/lsblk \
    && ln -s /chroot/chroot-host-wrapper.sh /chroot/mkfs.ext4 \
    && ln -s /chroot/chroot-host-wrapper.sh /chroot/mkfs.xfs \
    && ln -s /chroot/chroot-host-wrapper.sh /chroot/resize2fs \
    && ln -s /chroot/chroot-host-wrapper.sh /chroot/xfs_growfs \
    && ln -s /chroot/chroot-host-wrapper.sh /chroot/dumpe2fs \
    && ln -s /chroot/chroot-host-wrapper.sh /chroot/xfs_io \
    && ln -s /chroot/chroot-host-wrapper.sh /chroot/stat \
    && ln -s /chroot/chroot-host-wrapper.sh /chroot/free

ENV PATH="/chroot:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

ENTRYPOINT [ "/entrypoint.sh" ]
