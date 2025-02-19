// Copyright 2018 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package member

import (
	"fmt"

	"github.com/pingcap/advanced-statefulset/client/apis/apps/v1/helper"
	"github.com/pingcap/tidb-operator/pkg/apis/pingcap/v1alpha1"
	"github.com/pingcap/tidb-operator/pkg/controller"
	apps "k8s.io/api/apps/v1"
	"k8s.io/klog"
)

type pdUpgrader struct {
	deps *controller.Dependencies
}

// NewPDUpgrader returns a pdUpgrader
func NewPDUpgrader(deps *controller.Dependencies) Upgrader {
	return &pdUpgrader{
		deps: deps,
	}
}

func (u *pdUpgrader) Upgrade(tc *v1alpha1.TidbCluster, oldSet *apps.StatefulSet, newSet *apps.StatefulSet) error {
	return u.gracefulUpgrade(tc, oldSet, newSet)
}

func (u *pdUpgrader) gracefulUpgrade(tc *v1alpha1.TidbCluster, oldSet *apps.StatefulSet, newSet *apps.StatefulSet) error {
	ns := tc.GetNamespace()
	tcName := tc.GetName()
	if !tc.Status.PD.Synced {
		return fmt.Errorf("tidbcluster: [%s/%s]'s pd status sync failed, can not to be upgraded", ns, tcName)
	}
	if tc.Status.TiCDC.Phase == v1alpha1.UpgradePhase ||
		tc.PDScaling() {
		klog.Infof("TidbCluster: [%s/%s]'s ticdc status is %v, "+
			"pd status is %v, can not upgrade pd",
			ns, tcName, tc.Status.TiCDC.Phase,
			tc.Status.PD.Phase)
		_, podSpec, err := GetLastAppliedConfig(oldSet)
		if err != nil {
			return err
		}
		newSet.Spec.Template.Spec = *podSpec
		return nil
	}

	tc.Status.PD.Phase = v1alpha1.UpgradePhase
	if !templateEqual(newSet, oldSet) {
		return nil
	}

	if tc.Status.PD.StatefulSet.UpdateRevision == tc.Status.PD.StatefulSet.CurrentRevision {
		return nil
	}

	if oldSet.Spec.UpdateStrategy.Type == apps.OnDeleteStatefulSetStrategyType || oldSet.Spec.UpdateStrategy.RollingUpdate == nil {
		// Manually bypass tidb-operator to modify statefulset directly, such as modify pd statefulset's RollingUpdate straregy to OnDelete strategy,
		// or set RollingUpdate to nil, skip tidb-operator's rolling update logic in order to speed up the upgrade in the test environment occasionally.
		// If we encounter this situation, we will let the native statefulset controller do the upgrade completely, which may be unsafe for upgrading pd.
		// Therefore, in the production environment, we should try to avoid modifying the pd statefulset update strategy directly.
		newSet.Spec.UpdateStrategy = oldSet.Spec.UpdateStrategy
		klog.Warningf("tidbcluster: [%s/%s] pd statefulset %s UpdateStrategy has been modified manually", ns, tcName, oldSet.GetName())
		return nil
	}

	setUpgradePartition(newSet, *oldSet.Spec.UpdateStrategy.RollingUpdate.Partition)
	podOrdinals := helper.GetPodOrdinals(*oldSet.Spec.Replicas, oldSet).List()
	for _i := len(podOrdinals) - 1; _i >= 0; _i-- {
		i := podOrdinals[_i]
		podName := PdPodName(tcName, i)
		pod, err := u.deps.PodLister.Pods(ns).Get(podName)
		if err != nil {
			return fmt.Errorf("gracefulUpgrade: failed to get pods %s for cluster %s/%s, error: %s", podName, ns, tcName, err)
		}

		revision, exist := pod.Labels[apps.ControllerRevisionHashLabelKey]
		if !exist {
			return controller.RequeueErrorf("tidbcluster: [%s/%s]'s pd pod: [%s] has no label: %s", ns, tcName, podName, apps.ControllerRevisionHashLabelKey)
		}

		if revision == tc.Status.PD.StatefulSet.UpdateRevision {
			if member, exist := tc.Status.PD.Members[PdName(tc.Name, i, tc.Namespace, tc.Spec.ClusterDomain)]; !exist || !member.Health {
				return controller.RequeueErrorf("tidbcluster: [%s/%s]'s pd upgraded pod: [%s] is not ready", ns, tcName, podName)
			}
			continue
		}

		if u.deps.CLIConfig.PodWebhookEnabled {
			setUpgradePartition(newSet, i)
			return nil
		}

		return u.upgradePDPod(tc, i, newSet)
	}

	return nil
}

func (u *pdUpgrader) upgradePDPod(tc *v1alpha1.TidbCluster, ordinal int32, newSet *apps.StatefulSet) error {
	ns := tc.GetNamespace()
	tcName := tc.GetName()
	upgradePdName := PdName(tcName, ordinal, tc.Namespace, tc.Spec.ClusterDomain)
	upgradePodName := PdPodName(tcName, ordinal)
	if tc.Status.PD.Leader.Name == upgradePdName || tc.Status.PD.Leader.Name == upgradePodName {
		var targetName string
		if tc.PDStsActualReplicas() > 1 {
			targetOrdinal := helper.GetMaxPodOrdinal(*newSet.Spec.Replicas, newSet)
			if ordinal == targetOrdinal {
				targetOrdinal = helper.GetMinPodOrdinal(*newSet.Spec.Replicas, newSet)
			}
			targetName = PdName(tcName, targetOrdinal, tc.Namespace, tc.Spec.ClusterDomain)
			if _, exist := tc.Status.PD.Members[targetName]; !exist {
				targetName = PdPodName(tcName, targetOrdinal)
			}
		} else {
			for _, member := range tc.Status.PD.PeerMembers {
				if member.Name != upgradePdName && member.Health {
					targetName = member.Name
					break
				}
			}
		}
		if len(targetName) > 0 {
			err := u.transferPDLeaderTo(tc, targetName)
			if err != nil {
				klog.Errorf("pd upgrader: failed to transfer pd leader to: %s, %v", targetName, err)
				return err
			}
			klog.Infof("pd upgrader: transfer pd leader to: %s successfully", targetName)
			return controller.RequeueErrorf("tidbcluster: [%s/%s]'s pd member: [%s] is transferring leader to pd member: [%s]", ns, tcName, upgradePdName, targetName)
		}
	}
	setUpgradePartition(newSet, ordinal)
	return nil
}

func (u *pdUpgrader) transferPDLeaderTo(tc *v1alpha1.TidbCluster, targetName string) error {
	return controller.GetPDClient(u.deps.PDControl, tc).TransferPDLeader(targetName)
}

type fakePDUpgrader struct{}

// NewFakePDUpgrader returns a fakePDUpgrader
func NewFakePDUpgrader() Upgrader {
	return &fakePDUpgrader{}
}

func (u *fakePDUpgrader) Upgrade(tc *v1alpha1.TidbCluster, _ *apps.StatefulSet, _ *apps.StatefulSet) error {
	if !tc.Status.PD.Synced {
		return fmt.Errorf("tidbcluster: pd status sync failed, can not to be upgraded")
	}
	tc.Status.PD.Phase = v1alpha1.UpgradePhase
	return nil
}
