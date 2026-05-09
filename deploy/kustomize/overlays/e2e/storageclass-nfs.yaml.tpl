apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: fileblock
provisioner: fileblock.csi
parameters:
  backingStore.type: nfs
  backingStore.nfs.server: 127.0.0.1
  backingStore.nfs.path: /tmp/fileblock-e2e-export
  backingStore.nfs.mountOptions: "nfsvers=${NFS_VERSION},hard,timeo=600"
reclaimPolicy: Delete
allowVolumeExpansion: true
volumeBindingMode: WaitForFirstConsumer
