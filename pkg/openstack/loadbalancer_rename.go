/*
Copyright 2023 The Kubernetes Authors.

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

package openstack

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/gophercloud/gophercloud"

	"k8s.io/klog/v2"

	"github.com/gophercloud/gophercloud/openstack/loadbalancer/v2/listeners"
	"github.com/gophercloud/gophercloud/openstack/loadbalancer/v2/loadbalancers"
	"github.com/gophercloud/gophercloud/openstack/loadbalancer/v2/monitors"
	"github.com/gophercloud/gophercloud/openstack/loadbalancer/v2/pools"
	openstackutil "k8s.io/cloud-provider-openstack/pkg/util/openstack"
)

// lbHasOldClusterName checks if the OCCM LB prefix is present and if so, validates the cluster-name
// component value. Returns true if the cluster-name component of the loadbalancer's name doesn't match
// clusterName.
func lbHasOldClusterName(loadbalancer *loadbalancers.LoadBalancer, clusterName string) bool {
	if !strings.HasPrefix(loadbalancer.Name, servicePrefix) {
		// This one was probably not created by OCCM, let's leave it as is.
		return false
	}
	existingClusterName, _, _ := decomposeLBName("", loadbalancer.Name)
	klog.Errorf("lbHasOldClusterName! existingClusterName=%s", existingClusterName)
	if existingClusterName != clusterName {
		// This one looks like it has wrong clusterName
		return true
	}
	return false
}

// decomposeLBName returns clusterName, namespace and name based on LB name
func decomposeLBName(resourcePrefix, lbName string) (string, string, string) {
	// TODO(dulek): Handle cases when this is cut at 255
	lbNameRegex := regexp.MustCompile(fmt.Sprintf("%s%s(.+)_([^_]+)_([^_]+)", resourcePrefix, servicePrefix)) // this is static

	matches := lbNameRegex.FindAllStringSubmatch(lbName, -1)
	if matches == nil {
		return "", "", ""
	}
	return matches[0][1], matches[0][2], matches[0][3]
}

// decomposeLBName returns clusterName, namespace and name based on LB name
func replaceClusterName(oldClusterName, clusterName, objectName string) string {
	return strings.Replace(objectName, oldClusterName, clusterName, 1)
}

// renameLoadBalancer renames all the children and then the LB itself to match new lbName.
// The purpose is handling a change of clusterName.
func renameLoadBalancer(client *gophercloud.ServiceClient, loadbalancer *loadbalancers.LoadBalancer, lbName, clusterName string) (*loadbalancers.LoadBalancer, error) {
	lbListeners, err := openstackutil.GetListenersByLoadBalancerID(client, loadbalancer.ID)
	if err != nil {
		return nil, err
	}
	for _, listener := range lbListeners {
		if !strings.HasPrefix(listener.Name, listenerPrefix) {
			// It doesn't seem to be ours, let's not touch it.
			continue
		}

		oldClusterName, _, _ := decomposeLBName(fmt.Sprintf("%s[0-9]+_", listenerPrefix), listener.Name)

		if oldClusterName != clusterName {
			// First let's handle pool which we assume is a child of the listener. Only one pool per one listener.
			lbPool, err := openstackutil.GetPoolByListener(client, loadbalancer.ID, listener.ID)
			if err != nil {
				return nil, err
			}
			oldClusterName, _, _ = decomposeLBName(fmt.Sprintf("%s[0-9]+_", poolPrefix), lbPool.Name)
			if oldClusterName != clusterName {
				if lbPool.MonitorID != "" {
					monitor, err := openstackutil.GetHealthMonitor(client, lbPool.MonitorID)
					if err != nil {
						return nil, err
					}
					oldClusterName, _, _ := decomposeLBName(fmt.Sprintf("%s[0-9]+_", monitorPrefix), monitor.Name)
					if oldClusterName != clusterName {
						monitor.Name = replaceClusterName(oldClusterName, clusterName, monitor.Name)
						err = openstackutil.UpdateHealthMonitor(client, monitor.ID, monitors.UpdateOpts{Name: &monitor.Name}, loadbalancer.ID)
						if err != nil {
							return nil, err
						}
					}
				}

				lbPool.Name = replaceClusterName(oldClusterName, clusterName, lbPool.Name)
				err = openstackutil.UpdatePool(client, loadbalancer.ID, lbPool.ID, pools.UpdateOpts{Name: &lbPool.Name})
				if err != nil {
					return nil, err
				}
			}

			for i, tag := range listener.Tags {
				// There might be tags for shared listeners, that's why we analyze each tag on its own.
				oldClusterNameTag, _, _ := decomposeLBName("", tag)
				if oldClusterNameTag != "" && oldClusterNameTag != clusterName {
					listener.Tags[i] = replaceClusterName(oldClusterNameTag, clusterName, tag)
				}
			}
			listener.Name = replaceClusterName(oldClusterName, clusterName, listener.Name)
			err = openstackutil.UpdateListener(client, loadbalancer.ID, listener.ID, listeners.UpdateOpts{Name: &listener.Name, Tags: &listener.Tags})
			if err != nil {
				return nil, err
			}
		}
	}

	// At last we rename the LB. This is to make sure we only stop retrying to rename the LB once all of the children
	// are handled.
	for i, tag := range loadbalancer.Tags {
		// There might be tags for shared lbs, that's why we analyze each tag on its own.
		oldClusterNameTag, _, _ := decomposeLBName("", tag)
		if oldClusterNameTag != "" && oldClusterNameTag != clusterName {
			loadbalancer.Tags[i] = replaceClusterName(oldClusterNameTag, clusterName, tag)
		}
	}
	return openstackutil.UpdateLoadBalancer(client, loadbalancer.ID, loadbalancers.UpdateOpts{Name: &lbName, Tags: &loadbalancer.Tags})
}
