# gcp-filestore-csi-driver
[Google Cloud Filestore](https://cloud.google.com/filestore) CSI driver for
use in Kubernetes and other container orchestrators.

Disclaimer: This is not an officially supported Google product.

## Project Overview
This driver allows volumes backed by Google Cloud Filestore instances to be
dynamically created and mounted by workloads.

## Project Status
Status: Beta

Latest image: `gcr.io/k8s-staging-cloud-provider-gcp/gcp-filestore-csi-driver:v0.3.0`

Also see [known issues](KNOWN_ISSUES.md) and [CHANGELOG](CHANGELOG.md).

### CSI Compatibility
This plugin is compatible with CSI version 1.3.0.

### Kubernetes Compatibility

| Filestore CSI Driver\Kubernetes Version | 1.14 | 1.15 | 1.16 | 1.17+ |
| --------------------------------------- | ---- | ---- | ---- | ----- |
| v0.2.0 (alpha)                          | yes  |  no  |  no  |  no   |
| v0.3.0 (beta)                           | no   |  no  |  no  |  yes  |
| master                                  | no   |  no  |  no  |  yes  |

## Plugin Features

### Supported CreateVolume parameters
This version of the driver creates a new Cloud Filestore instance per
volume. Customizable parameters for volume creation include:

| Parameter         | Values                  | Default                                | Description |
| ---------------   | ----------------------- |-----------                             | ----------- |
| tier              | "standard"<br>"premium" | "standard"                             | storage performance tier |
| network           | string                  | "default"                              | VPC name |
| reserved-ipv4-cidr| string		              | ""                                     | CIDR range to allocate Filestore IP Ranges from.<br>The CIDR must be large enough to accommodate multiple Filestore IP Ranges of /29 each |

For Kubernetes clusters, these parameters are specified in the StorageClass.

Note that non-default networks require extra [firewall setup](https://cloud.google.com/filestore/docs/configuring-firewall)

## Current supported Features
* Volume resizing: CSI Filestore driver supports volume expansion for all supported Filestore tiers. See user-guide [here](docs/kubernetes/resize.md). Volume expansion feature is beta in kubernetes 1.16+.
* Labels: Filestore supports labels per instance, which is a map of key value pairs. Filestore CSI driver enables user provided labels
  to be stamped on the instance. User can provide labels by using 'labels' key in StorageClass.parameters. In addition, Filestore instance can
  be labelled with information about what PVC/PV the instance was created for. To obtain the PVC/PV information, '--extra-create-metadata' flag needs to be set on the CSI external-provisioner sidecar. User provided label keys and values must comply with the naming convention as specified [here](https://cloud.google.com/resource-manager/docs/creating-managing-labels#requirements). Please see [this](examples/kubernetes/sc-labels.yaml) storage class examples to apply custom user-provided labels to the Filestore instance.
* Topology preferences: Filestore performance and network usage is affected by topology. For example, it is recommended to run
  workloads in the same zone where the Cloud Filestore instance is provisioned in. The following table describes how provisioning can be tuned by topology. The volumeBindingMode is specified in the StorageClass used for provisioning. 'strict-topology' is a flag passed to the CSI provisioner sidecar. 'allowedTopology' is also specified in the StorageClass. The Filestore driver will use the first topology in the preferred list, or if empty the first in the requisite list. If topology feature is not enabled in CSI provisioner (--feature-gates=Topology=false), CreateVolume.accessibility_requirements will be nil, and the driver simply creates the instance in the zone where the driver deployment running. See user-guide [here](docs/kubernetes/topology.md). Topology feature is GA in kubernetes 1.17+.


  | SC Bind Mode         | 'strict-topology' | SC allowedTopology  | CSI provisioner Behavior    |
  | -------------------- | ----------------- | ------------------- | --------------------------- |
  | WaitForFirstCustomer |       true        |        Present      | If the topology of the node selected by the schedule is not in allowedTopology, provisioning fails and the scheduler will continue with a different node. Otherwise, CreateVolume is called with requisite and preferred topologies set to that of the selected node |
  | WaitForFirstCustomer |       false       |        Present      | If the topology of the node selected by the schedule is not in allowedTopology, provisioning fails and the scheduler will continue with a different node. Otherwise, CreateVolume is called with requisite set to allowedTopology and preferred set to allowedTopology rearranged with the selected node topology as the first parameter |
  | WaitForFirstCustomer |       true        |        Not Present  | Call CreateVolume with requisite set to selected node topology, and preferred set to the same |
  | WaitForFirstCustomer |       false       |        Not Present  | Call CreateVolume with requisite set to aggregated topology across all nodes, which matches the topology of the selected node, and preferred is set to the sorted and shifted version of requisite, with selected node topology as the first parameter |
  | Immediate            |       N/A         |        Present      | Call CreateVolume with requisite set to allowedTopology and preferred set to the sorted and shifted version of requisite at a randomized index |
  | Immediate            |       N/A         |        Not Present  | Call CreateVolume with requisite = aggregated topology across nodes which contain the topology keys of CSINode objects, preferred = sort and shift requisite at a randomized index |

* Volume Snapshot: The CSI driver currently supports CSI VolumeSnapshots on a GCP Filestore instance using the GCP Filestore Backup feature. CSI VolumeSnapshot is a Beta feature in k8s enabled by default in 1.17+. The GCP Filestore Snapshot [alpha](https://cloud.google.com/sdk/gcloud/reference/alpha/filestore/snapshots/create) is not currently supported, but will be in the future via the type parameter in the VolumeSnapshotClass. For more details see the user-guide [here](docs/kubernetes/backup.md).
* Volume Restore: The CSI driver supports out-of-place restore of new GCP Filestore instance from a given GCP Filestore Backup. See user-guide restore steps [here](docs/kubernetes/backup.md) and GCP Filestore Backup restore documentation [here](https://cloud.google.com/filestore/docs/backup-restore). This feature needs kubernetes 1.17+.
* Pre-provisioned Filestore instance: Pre-provisioned filestore instances can be leveraged and consumed by workloads by mapping a given filestore instance to a PersistentVolume and PersistentVolumeClaim. See user-guide [here](docs/kubernetes/pre-provisioned-pv.md) and filestore documentation [here](https://cloud.google.com/filestore/docs/accessing-fileshares)

## Future Features
* Non-root access: By default, GCFS instances are only writable by the root user
  and readable by all users. Provide a CreateVolume parameter to set non-root
  owners.
* Subdirectory provisioning: Given an existing Cloud Filestore instance, provision a
  subdirectory as a volume. This provisioning mode does not provide capacity
  isolation. Quota support needs investigation. For now, the
  [nfs-client](https://github.com/kubernetes-incubator/external-storage/tree/master/nfs-client)
  external provisioner can be used to provide similar functionality for
  Kubernetes clusters.
* FsGroup feature: [CSIVolumeFSGroupPolicy](https://kubernetes-csi.github.io/docs/csi-driver-object.html) is a Kubernetes feature in Beta is 1.20, which allows CSI drivers to opt into FSGroup policies. Filestore CSI driver plans to support it in near future. As a workaround, until the feature is available, see user-guide [here](docs/kubernetes/fsgroup.md) on how to apply fsgroup to volumes backed by filestore instances.

## Kubernetes Development

* The first step would be create a service account with appropriate role bindings. For `dev` [overlay](deploy/kubernetes/overlays/dev) the script [project_setup.sh](deploy/project_setup.sh) creates a service acount gcp-filestore-csi-driver-sa@<your-gcp-project>.iam.gserviceaccount.com and grants roles/file.editor, roles/editor role to the service account.

```$ PROJECT=<your-gcp-project> DEPLOY_VERSION=dev ./deploy/project_setup.sh```

* Else, for any other overlay, point $GCFS_SA_DIR to a directory to store service account key. project_setup.sh creates `gcp-filestore-csi-driver-sa@<your-gcp-project>.iam.gserviceaccount.com` and grants roles/file.editor, roles/editor role to the service account and downloads the key to the $GCFS_SA_DIR/gcp_filestore_csi_driver_sa.json.
```$ PROJECT=<your-gcp-project> GCFS_SA_DIR=<your-directory-to-store-credentials-by-default-home-dir> ./deploy/project_setup.sh```

* To build the Filestore CSI latest driver image and push to a container registry.
```$ PROJECT=<your-gcp-project> make build-image-and-push```

* The base manifests like core driver manifests, rbac role bindings are listed under [here](deploy/kubernetes/base).
  The overlays (e.g prow-gke-release-staging-head, prow-gke-release-staging-rc, stable, dev) are listed under deploy/kubernetes/overlays
  apply transformations on top of the base manifests.

* 'dev' overlay uses default service account for communicating with GCP services. `https://www.googleapis.com/auth/cloud-platform` scope allows full access to all Google Cloud APIs and given node scope will allow any pod to reach GCP services as the provided service account, and so should only be used for testing and development, not production clusters. cluster_setup.sh installs kustomize and creates the driver manifests package and deploys to the cluster. Bring up GCE cluster with following:
```$ NODE_SCOPES=https://www.googleapis.com/auth/cloud-platform KUBE_GCE_NODE_SERVICE_ACCOUNT=<SERVICE_ACCOUNT_NAME>@$PROJECT.iam.gserviceaccount.com kubetest --up```

* Deploy the driver as follows.

For a `dev` overlay,
```$ PROJECT=<your-gcp-project> DEPLOY_VERSION=dev ./deploy/kubernetes/cluster_setup.sh```

For a non `dev` overlay
```$ PROJECT=<your-gcp-project> DEPLOY_VERSION=<any-non-dev-overlay> GCFS_SA_DIR=<your-directory-to-store-credentials-by-default-home-dir> ./deploy/kubernetes/cluster_setup.sh ```

* For cleanup of the driver run the following:
```$ PROJECT=<your-gcp-project> DEPLOY_VERSION=dev ./deploy/kubernetes/cluster_cleanup.sh```

## Gcloud Application Default Credentials and scopes
See [here](https://cloud.google.com/docs/authentication/production), [here](https://cloud.google.com/compute/docs/access/create-enable-service-accounts-for-instances) and [here](https://cloud.google.com/storage/docs/authentication#oauth-scopes)

## Filestore IAM roles and permissions
See [here](https://cloud.google.com/filestore/docs/access-control#iam-access)