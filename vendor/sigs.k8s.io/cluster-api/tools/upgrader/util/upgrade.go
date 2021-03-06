/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package util

import (
	"fmt"
	"time"

	"github.com/golang/glog"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	clusterv1 "sigs.k8s.io/cluster-api/pkg/apis/cluster/v1alpha1"
	clientv1alpha1 "sigs.k8s.io/cluster-api/pkg/client/clientset_generated/clientset/typed/cluster/v1alpha1"
	"sigs.k8s.io/cluster-api/pkg/clientcmd"
	"sigs.k8s.io/cluster-api/pkg/util"
)

var (
	kubeClientSet *kubernetes.Clientset
	client        clientv1alpha1.ClusterV1alpha1Interface
	machInterface clientv1alpha1.MachineInterface
)

func initClient(kubeconfig string) error {
	if kubeconfig == "" {
		kubeconfig = util.GetDefaultKubeConfigPath()
	}
	coreClientset, clusterapiClientset, err := clientcmd.NewClientsForDefaultSearchpath(kubeconfig, clientcmd.NewConfigOverrides())
	if err != nil {
		glog.Fatalf("Error creating rest config: %v", err)
		return err
	}
	kubeClientSet = coreClientset
	client = clusterapiClientset.ClusterV1alpha1()
	machInterface = client.Machines(v1.NamespaceDefault)

	return nil
}

func checkMachineReady(machineName string, kubeVersion string) (bool, error) {
	machine, err := machInterface.Get(machineName, metav1.GetOptions{})
	if err != nil {
		return false, err
	}

	if machine.Status.NodeRef == nil {
		return false, nil
	}

	// Find the node object via reference in machine object.
	node, err := kubeClientSet.CoreV1().Nodes().Get(machine.Status.NodeRef.Name, metav1.GetOptions{})
	switch {
	case err != nil:
		glog.V(1).Infof("Failed to get node %s: %v", machineName, err)
		return false, err
	case !util.IsNodeReady(node):
		glog.V(1).Infof("node %s is not ready. Status : %v", machineName, node.Status.Conditions)
		return false, nil
	case node.Status.NodeInfo.KubeletVersion == "v"+kubeVersion:
		glog.Infof("node %s is ready", machineName)
		return true, nil
	default:
		glog.V(1).Infof("node %s kubelet current version: %s, target: %s.", machineName, node.Status.NodeInfo.KubeletVersion, kubeVersion)
		return false, nil
	}
}

func UpgradeCluster(kubeversion string, kubeconfig string) error {
	glog.Infof("Starting to upgrade cluster to version: %s", kubeversion)

	if err := initClient(kubeconfig); err != nil {
		return err
	}

	machine_list, err := machInterface.List(metav1.ListOptions{})
	if err != nil {
		return err
	}

	glog.Info("Upgrading the control plane.")

	// Update the control plan first. It assumes single master.
	var master *clusterv1.Machine = nil
	for _, mach := range machine_list.Items {
		if util.IsMaster(&mach) {
			master = &mach
			break
		}
	}

	if master == nil {
		err = fmt.Errorf("No master is found.")
	} else {
		master.Spec.Versions.Kubelet = kubeversion
		master.Spec.Versions.ControlPlane = kubeversion
		new_machine, err := machInterface.Update(master)
		if err == nil {
			err = wait.Poll(5*time.Second, 10*time.Minute, func() (bool, error) {
				ready, err := checkMachineReady(new_machine.Name, kubeversion)
				if err != nil {
					// Ignore the error as control plan is restarting.
					return false, nil
				}
				return ready, nil
			})
		}
	}

	if err != nil {
		return err
	}

	glog.Info("Finished upgrading control plane.")

	num_nodes := len(machine_list.Items) - 1
	glog.Infof("Upgrading %d nodes in the cluster.", num_nodes)

	// Continue to update all the nodes.
	errors := make(chan error, len(machine_list.Items))
	for i, _ := range machine_list.Items {
		if !util.IsMaster(&machine_list.Items[i]) {
			go func(mach *clusterv1.Machine) {
				glog.Infof("Upgrading %s.", mach.Name)
				mach, err = machInterface.Get(mach.Name, metav1.GetOptions{})
				if err == nil {
					mach.Spec.Versions.Kubelet = kubeversion
					new_machine, err := machInterface.Update(mach)
					if err == nil {
						// Polling the cluster until nodes are updated.
						err = wait.Poll(5*time.Second, 10*time.Minute, func() (bool, error) {
							ready, err := checkMachineReady(new_machine.Name, kubeversion)
							if err != nil {
								// Ignore the error as control plan is restarting.
								return false, nil
							}
							return ready, nil
						})
					} else {
						glog.Errorf("Update to machine object (%s) failed : %v", mach.Name, err)
					}
				} else {
					glog.Errorf("client.Machines().Get() failed : %v", err)
				}
				errors <- err
			}(&machine_list.Items[i])
		}
	}

	for _, machine := range machine_list.Items {
		if !util.IsMaster(&machine) {
			if err = <-errors; err != nil {
				return err
			} else {
				num_nodes--
				if num_nodes > 0 {
					glog.Infof("%d nodes are still being updated", num_nodes)
				}
			}
		}
	}

	glog.Info("Successfully upgraded the cluster.")
	return nil
}
