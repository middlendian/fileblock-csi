apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: fileblock
provisioner: fileblock.csi
parameters:
  backingStore.type: nfs
  backingStore.nfs.server: ${NFS_SERVER}
  backingStore.nfs.path: ${NFS_EXPORT}
  backingStore.nfs.mountOptions: "nfsvers=${NFS_VERSION},hard,timeo=600"
reclaimPolicy: Delete
allowVolumeExpansion: true
volumeBindingMode: WaitForFirstConsumer
