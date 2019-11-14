# octant and antrea-octant-plugin installation

## Overview

There are two ways to deploy octant and antrea-octant-plugin.

* Deploy octant and antrea-octant-plugin as a process (out of Kubernetes cluster).

* Deploy octant and antrea-octant-plugin as a pod (inside Kubernetes cluster).

### Before you start
antrea-octant-plugin depends on monitoring CRDs, AntreaControllerInfo and AntreaAgentInfo.

To run octant together with antrea-octant-plugin, please make sure you have these two CRDs defined in you K8s cluster.

If not, please refer to [antrea.yaml](https://github.com/vmware-tanzu/antrea/blob/master/build/yamls/antrea.yml) to
create these two CRDs first.

### 1. Deploy octant and antrea-octant-plugin as a process

Refer to [octant README](https://github.com/vmware-tanzu/octant/blob/master/README.md#installation) for 
detailed installation instructions.

You can follow the steps listed below to install octant and antrea-octant-plugin on linux.

1.Get and install octant v0.8.0.

Depending on your linux operation system, to install octant v0.8.0, you can use either

```
wget https://github.com/vmware-tanzu/octant/releases/download/v0.8.0/octant_0.8.0_Linux-64bit.deb

dpkg -i octant_0.8.0_Linux-64bit.deb
```
or
```
wget https://github.com/vmware-tanzu/octant/releases/download/v0.8.0/octant_0.8.0_Linux-64bit.rpm

rpm -i octant_0.8.0_Linux-64bit.rpm
```

2.Export your kubeconfig path (file location depends on your setup) to environment variable $KUBECONFIG.

```
export KUBECONFIG=/etc/kubernetes/admin.conf
```

3.Build antrea-octant-plugin.

```
make bin
```

4.Move antrea-octant-plugin to OCTANT_PLUGIN_PATH.

```
# If you did not change OCTANT_PLUGIN_PATH, the default folder should be $HOME/.config/octant/plugins.

mv antrea/bin/antrea-octant-plugin $HOME/.config/octant/plugins/
```

5.Start octant as a background process with UI related environment variables.

```
# Change port 80 according to your environment and set OCTANT_ACCEPTED_HOSTS according to your demand

OCTANT_LISTENER_ADDR=0.0.0.0:80 OCTANT_ACCEPTED_HOSTS=0.0.0.0 OCTANT_DISABLE_OPEN_BROWSER=true nohup octant &
```

Now, you are supposed to see octant is running together with antrea-octant-plugin via url http://(IP or $HOSTNAME):80.


### 2. Deploy octant and antrea-octant-plugin as a pod

To deploy octant in pod, please refer to the example [Running octant in cluster](https://github.com/vmware-tanzu/octant/tree/master/examples/in-cluster)

Following the example above, you need to execute the commands below in order to run octant and antrea-octant-plugin in pod.

1. Build octant-antrea-ubuntu docker image.

```
make octant-antrea-ubuntu
```

2.Create a secret that contains your kubeconfig.

```
# Change --from-file according to kubeconfig location in your set up.

kubectl create secret generic octant-kubeconfig --from-file=/etc/kubernetes/admin.conf -n kube-system
```

3.You may need to update build/yamls/antrea-octant.yml according to your kubeconfig file name.

4.You may need to label certain node if you want to run octant Pod on a fixed node so that you can access UI via a fixed URL.

```
kubectl label nodes $HOSTNAME role=antrea-octant.
```

5.Apply the deployment.

```
kubectl apply -f build/yamls/antrea-octant.yml
```

Now, you are supposed to see octant is running together with antrea-octant-plugin via url http://(IP or $HOSTNAME):80.
