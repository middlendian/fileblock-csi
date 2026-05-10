apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: fileblock
provisioner: fileblock.csi
parameters:
  backingStore.type: nfs
  backingStore.nfs.server: ${NFS_SERVER}
  backingStore.nfs.path: ${NFS_EXPORT}
  # nolock is required for NFSv3 (the driver pod doesn't run rpc.statd
  # and fileblock doesn't depend on NFS-level locks — see README).
  # NFSv4 has integrated locking and ignores nolock, so the same
  # template works for both versions.
  backingStore.nfs.mountOptions: "nfsvers=${NFS_VERSION},hard,timeo=600,nolock"
reclaimPolicy: Delete
allowVolumeExpansion: true
volumeBindingMode: WaitForFirstConsumer
