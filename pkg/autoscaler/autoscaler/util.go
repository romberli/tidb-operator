// Copyright 2020 PingCAP, Inc.
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

package autoscaler

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/pingcap/tidb-operator/pkg/apis/pingcap/v1alpha1"
	"github.com/pingcap/tidb-operator/pkg/controller"
	"github.com/pingcap/tidb-operator/pkg/label"
	"github.com/pingcap/tidb-operator/pkg/pdapi"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog"
	"k8s.io/utils/pointer"
)

var zeroQuantity = resource.MustParse("0")

// checkAutoScaling would check whether an autoscaling for a group is permitted
func checkAutoScaling(tac *v1alpha1.TidbClusterAutoScaler, memberType v1alpha1.MemberType, group string, beforeReplicas, afterReplicas int32) bool {
	if beforeReplicas > afterReplicas {
		switch memberType {
		case v1alpha1.TiKVMemberType:
			return checkAutoScalingInterval(tac, *tac.Spec.TiKV.ScaleInIntervalSeconds, memberType, group)
		case v1alpha1.TiDBMemberType:
			return checkAutoScalingInterval(tac, *tac.Spec.TiDB.ScaleInIntervalSeconds, memberType, group)
		}
	} else if beforeReplicas < afterReplicas {
		switch memberType {
		case v1alpha1.TiKVMemberType:
			return checkAutoScalingInterval(tac, *tac.Spec.TiKV.ScaleOutIntervalSeconds, memberType, group)
		case v1alpha1.TiDBMemberType:
			return checkAutoScalingInterval(tac, *tac.Spec.TiDB.ScaleOutIntervalSeconds, memberType, group)
		}
	}
	return true
}

// checkAutoScalingInterval would check whether there is enough interval duration between every two auto-scaling
func checkAutoScalingInterval(tac *v1alpha1.TidbClusterAutoScaler, intervalSeconds int32, memberType v1alpha1.MemberType, group string) bool {
	var lastAutoScalingTimestamp *metav1.Time
	if memberType == v1alpha1.TiKVMemberType {
		status, existed := tac.Status.TiKV[group]
		if !existed {
			return true
		}
		lastAutoScalingTimestamp = status.LastAutoScalingTimestamp
	} else if memberType == v1alpha1.TiDBMemberType {
		status, existed := tac.Status.TiDB[group]
		if !existed {
			return true
		}
		lastAutoScalingTimestamp = status.LastAutoScalingTimestamp
	}
	if lastAutoScalingTimestamp == nil {
		return true
	}
	if intervalSeconds > int32(time.Since(lastAutoScalingTimestamp.Time).Seconds()) {
		return false
	}
	return true
}

func defaultResources(tc *v1alpha1.TidbCluster, tac *v1alpha1.TidbClusterAutoScaler, component v1alpha1.MemberType) {
	typ := fmt.Sprintf("default_%s", component.String())
	resource := v1alpha1.AutoResource{}
	var requests corev1.ResourceList

	switch component {
	case v1alpha1.TiDBMemberType:
		requests = tc.Spec.TiDB.Requests
	case v1alpha1.TiKVMemberType:
		requests = tc.Spec.TiKV.Requests
	}

	for res, v := range requests {
		switch res {
		case corev1.ResourceCPU:
			resource.CPU = v
		case corev1.ResourceMemory:
			resource.Memory = v
		case corev1.ResourceStorage:
			resource.Storage = v
		}
	}

	switch component {
	case v1alpha1.TiDBMemberType:
		if tac.Spec.TiDB.Resources == nil {
			tac.Spec.TiDB.Resources = make(map[string]v1alpha1.AutoResource)
		}
		tac.Spec.TiDB.Resources[typ] = resource
	case v1alpha1.TiKVMemberType:
		if tac.Spec.TiKV.Resources == nil {
			tac.Spec.TiKV.Resources = make(map[string]v1alpha1.AutoResource)
		}
		tac.Spec.TiKV.Resources[typ] = resource
	}
}

func defaultResourceTypes(tac *v1alpha1.TidbClusterAutoScaler, rule *v1alpha1.AutoRule, component v1alpha1.MemberType) {
	resources := getSpecResources(tac, component)
	if len(rule.ResourceTypes) == 0 {
		for name, res := range resources {
			// filtering resources which don't have storage when member type is TiKV during auto scaling.
			if component == v1alpha1.TiKVMemberType && res.Storage.Value() < 1 {
				continue
			}
			rule.ResourceTypes = append(rule.ResourceTypes, name)
		}
	}
	sort.Strings(rule.ResourceTypes)
}

func getBasicAutoScalerSpec(tac *v1alpha1.TidbClusterAutoScaler, component v1alpha1.MemberType) *v1alpha1.BasicAutoScalerSpec {
	switch component {
	case v1alpha1.TiDBMemberType:
		return &tac.Spec.TiDB.BasicAutoScalerSpec
	case v1alpha1.TiKVMemberType:
		return &tac.Spec.TiKV.BasicAutoScalerSpec
	}
	return nil
}

func getSpecResources(tac *v1alpha1.TidbClusterAutoScaler, component v1alpha1.MemberType) map[string]v1alpha1.AutoResource {
	switch component {
	case v1alpha1.TiDBMemberType:
		if tac.Spec.TiDB != nil {
			return tac.Spec.TiDB.Resources
		}
	case v1alpha1.TiKVMemberType:
		if tac.Spec.TiKV != nil {
			return tac.Spec.TiKV.Resources
		}
	}
	return nil
}

func defaultBasicAutoScaler(tac *v1alpha1.TidbClusterAutoScaler, component v1alpha1.MemberType) {
	spec := getBasicAutoScalerSpec(tac, component)

	if spec.ScaleOutIntervalSeconds == nil {
		spec.ScaleOutIntervalSeconds = pointer.Int32Ptr(300)
	}
	if spec.ScaleInIntervalSeconds == nil {
		spec.ScaleInIntervalSeconds = pointer.Int32Ptr(500)
	}

	if spec.External != nil {
		return
	}

	for res, rule := range spec.Rules {
		if res == corev1.ResourceCPU {
			if rule.MinThreshold == nil {
				rule.MinThreshold = pointer.Float64Ptr(0.1)
			}
		}
		defaultResourceTypes(tac, &rule, component)
		spec.Rules[res] = rule
	}
}

// If the minReplicas not set, the default value would be 1
// If the Metrics not set, the default metric will be set to 80% average CPU utilization.
// defaultTAC would default the omitted value
func defaultTAC(tac *v1alpha1.TidbClusterAutoScaler, tc *v1alpha1.TidbCluster) {
	if tac.Annotations == nil {
		tac.Annotations = map[string]string{}
	}

	// Construct default resource
	if tac.Spec.TiKV != nil && tac.Spec.TiKV.External == nil && len(tac.Spec.TiKV.Resources) == 0 {
		defaultResources(tc, tac, v1alpha1.TiKVMemberType)
	}

	if tac.Spec.TiDB != nil && tac.Spec.TiDB.External == nil && len(tac.Spec.TiDB.Resources) == 0 {
		defaultResources(tc, tac, v1alpha1.TiDBMemberType)
	}

	if tidb := tac.Spec.TiDB; tidb != nil {
		defaultBasicAutoScaler(tac, v1alpha1.TiDBMemberType)
	}

	if tikv := tac.Spec.TiKV; tikv != nil {
		defaultBasicAutoScaler(tac, v1alpha1.TiKVMemberType)
	}

}

func validateBasicAutoScalerSpec(tac *v1alpha1.TidbClusterAutoScaler, component v1alpha1.MemberType) error {
	spec := getBasicAutoScalerSpec(tac, component)

	if spec.External != nil {
		return nil
	}

	if len(spec.Rules) == 0 {
		return fmt.Errorf("no rules defined for component %s in %s/%s", component.String(), tac.Namespace, tac.Name)
	}
	resources := getSpecResources(tac, component)

	if component == v1alpha1.TiKVMemberType {
		for name, res := range resources {
			if res.Storage.Cmp(zeroQuantity) == 0 {
				return fmt.Errorf("resource %s defined for tikv does not have storage in %s/%s", name, tac.Namespace, tac.Name)
			}
		}
	}

	acceptableResources := map[corev1.ResourceName]struct{}{
		corev1.ResourceCPU:     {},
		corev1.ResourceStorage: {},
	}

	checkCommon := func(res corev1.ResourceName, rule v1alpha1.AutoRule) error {
		if _, ok := acceptableResources[res]; !ok {
			return fmt.Errorf("unknown resource type %s of %s in %s/%s", res.String(), component.String(), tac.Namespace, tac.Name)
		}
		if rule.MaxThreshold > 1.0 || rule.MaxThreshold < 0.0 {
			return fmt.Errorf("max_threshold (%v) should be between 0 and 1 for rule %s of %s in %s/%s", rule.MaxThreshold, res, component.String(), tac.Namespace, tac.Name)
		}
		if len(rule.ResourceTypes) == 0 {
			return fmt.Errorf("no resources provided for rule %s of %s in %s/%s", res, component.String(), tac.Namespace, tac.Name)
		}
		for _, resType := range rule.ResourceTypes {
			if _, ok := resources[resType]; !ok {
				return fmt.Errorf("unknown resource %s for %s in %s/%s", resType, component.String(), tac.Namespace, tac.Name)
			}
		}
		return nil
	}

	for res, rule := range spec.Rules {
		if err := checkCommon(res, rule); err != nil {
			return err
		}

		switch res {
		case corev1.ResourceCPU:
			if *rule.MinThreshold > 1.0 || *rule.MinThreshold < 0.0 {
				return fmt.Errorf("min_threshold (%v) should be between 0 and 1 for rule %s of %s in %s/%s", *rule.MinThreshold, res, component.String(), tac.Namespace, tac.Name)
			}
			if *rule.MinThreshold > rule.MaxThreshold {
				return fmt.Errorf("min_threshold (%v) > max_threshold (%v) for cpu rule of %s in %s/%s", *rule.MinThreshold, rule.MaxThreshold, component.String(), tac.Namespace, tac.Name)
			}
		case corev1.ResourceStorage:
			if *rule.MinThreshold > 1.0 || *rule.MinThreshold < 0.0 {
				return fmt.Errorf("min_threshold (%v) should be between 0 and 1 for rule %s of %s in %s/%s", *rule.MinThreshold, res, component.String(), tac.Namespace, tac.Name)
			}
			if *rule.MinThreshold > rule.MaxThreshold {
				return fmt.Errorf("min_threshold (%v) > max_threshold (%v) for storage rule of %s in %s/%s", *rule.MinThreshold, rule.MaxThreshold, component.String(), tac.Namespace, tac.Name)
			}
		}
	}

	return nil
}

func validateTAC(tac *v1alpha1.TidbClusterAutoScaler) error {
	if tac.Spec.TiDB != nil && tac.Spec.TiDB.External == nil && len(tac.Spec.TiDB.Resources) == 0 {
		return fmt.Errorf("no resources provided for tidb in %s/%s", tac.Namespace, tac.Name)
	}

	if tac.Spec.TiKV != nil && tac.Spec.TiKV.External == nil && len(tac.Spec.TiKV.Resources) == 0 {
		return fmt.Errorf("no resources provided for tikv in %s/%s", tac.Namespace, tac.Name)
	}

	if tidb := tac.Spec.TiDB; tidb != nil {
		err := validateBasicAutoScalerSpec(tac, v1alpha1.TiDBMemberType)
		if err != nil {
			return err
		}
	}

	if tikv := tac.Spec.TiKV; tikv != nil {
		err := validateBasicAutoScalerSpec(tac, v1alpha1.TiKVMemberType)
		if err != nil {
			return err
		}
	}

	return nil
}

func autoscalerToStrategy(tc *v1alpha1.TidbCluster, tac *v1alpha1.TidbClusterAutoScaler, component v1alpha1.MemberType, nodeCount uint64) *pdapi.Strategy {
	var (
		homogeneousResource  *pdapi.Resource
		homogeneousTiDBCount *uint64
		homogeneousTiKVCount *uint64
	)

	resources := getSpecResources(tac, component)
	strategy := &pdapi.Strategy{NodeCount: nodeCount}

	for typ, res := range resources {
		switch typ {
		case pdapi.HomogeneousTiDBResourceType:
			if res.Count != nil {
				count := uint64(*res.Count)
				homogeneousTiDBCount = &count
			}
		case pdapi.HomogeneousTiKVResourceType:
			if res.Count != nil {
				count := uint64(*res.Count)
				homogeneousTiKVCount = &count
			}
		default:
			r := &pdapi.Resource{
				CPU:          uint64(res.CPU.MilliValue()),
				Memory:       uint64(res.Memory.Value()),
				Storage:      uint64(res.Storage.Value()),
				ResourceType: typ,
			}
			if res.Count != nil {
				count := uint64(*res.Count)
				r.Count = &count
			}

			strategy.Resources = append(strategy.Resources, r)
		}
	}

	switch component {
	case v1alpha1.TiDBMemberType:
		tidbStorage := tc.Spec.TiDB.Limits[corev1.ResourceStorage]
		homogeneousResource = &pdapi.Resource{
			ResourceType: pdapi.HomogeneousTiDBResourceType,
			CPU:          uint64(tc.Spec.TiDB.Requests.Cpu().MilliValue()),
			Memory:       uint64(tc.Spec.TiDB.Requests.Memory().Value()),
			Storage:      uint64((&tidbStorage).Value()),
			Count:        homogeneousTiDBCount,
		}

		strategy.Rules = []*pdapi.Rule{autoRulesToStrategyRule(component.String(), tac.Spec.TiDB.Rules)}
	case v1alpha1.TiKVMemberType:
		tikvStorage := tc.Spec.TiKV.Limits[corev1.ResourceStorage]
		homogeneousResource = &pdapi.Resource{
			ResourceType: pdapi.HomogeneousTiKVResourceType,
			CPU:          uint64(tc.Spec.TiKV.Requests.Cpu().MilliValue()),
			Memory:       uint64(tc.Spec.TiKV.Requests.Memory().Value()),
			Storage:      uint64((&tikvStorage).Value()),
			Count:        homogeneousTiKVCount,
		}

		strategy.Rules = []*pdapi.Rule{autoRulesToStrategyRule(component.String(), tac.Spec.TiKV.Rules)}
	}

	strategy.Resources = append(strategy.Resources, homogeneousResource)

	return strategy
}

func autoRulesToStrategyRule(component string, rules map[corev1.ResourceName]v1alpha1.AutoRule) *pdapi.Rule {
	result := &pdapi.Rule{
		Component: component,
	}

	var homogeneousResourceType string
	switch component {
	case v1alpha1.TiKVMemberType.String():
		homogeneousResourceType = pdapi.HomogeneousTiKVResourceType
	case v1alpha1.TiDBMemberType.String():
		homogeneousResourceType = pdapi.HomogeneousTiDBResourceType
	default:
		klog.Warningf("unknown component %s", component)
	}

	for res, rule := range rules {
		switch res {
		case corev1.ResourceCPU:
			// For CPU rule, users should both specify max_threshold and min_threshold
			// Defaulting and validating make sure that the min_threshold is set
			result.CPURule = &pdapi.CPURule{
				MaxThreshold:  rule.MaxThreshold,
				MinThreshold:  *rule.MinThreshold,
				ResourceTypes: append(rule.ResourceTypes, homogeneousResourceType),
			}
		case corev1.ResourceStorage:
			result.StorageRule = &pdapi.StorageRule{
				MaxThreshold:  rule.MaxThreshold,
				MinThreshold:  *rule.MinThreshold,
				ResourceTypes: append(rule.ResourceTypes, homogeneousResourceType),
			}
		default:
			klog.Warningf("unknown resource type %v", res.String())
		}
	}
	return result
}

const autoClusterPrefix = "auto-"

func genAutoClusterName(tas *v1alpha1.TidbClusterAutoScaler, component string, labels map[string]string, resource v1alpha1.AutoResource) (string, error) {
	seed := map[string]interface{}{
		"namespace": tas.Namespace,
		"tas":       tas.Name,
		"component": component,
		"cpu":       resource.CPU.AsDec().UnscaledBig().Uint64(),
		"storage":   resource.Storage.AsDec().UnscaledBig().Uint64(),
		"memory":    resource.Memory.AsDec().UnscaledBig().Uint64(),
		"labels":    labels,
	}
	marshaled, err := json.Marshal(seed)
	if err != nil {
		return "", err
	}

	return autoClusterPrefix + v1alpha1.HashContents(marshaled), nil
}

func newAutoScalingCluster(tc *v1alpha1.TidbCluster, tac *v1alpha1.TidbClusterAutoScaler, autoTcName, component string) *v1alpha1.TidbCluster {
	autoTc := &v1alpha1.TidbCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      autoTcName,
			Namespace: tc.Namespace,
			Labels: map[string]string{
				label.BaseTCLabelKey:        tc.Name,
				label.AutoInstanceLabelKey:  tac.Name,
				label.AutoComponentLabelKey: component,
			},
			OwnerReferences: []metav1.OwnerReference{
				controller.GetTiDBClusterAutoScalerOwnerRef(tac),
			},
		},
		Spec: *tc.Spec.DeepCopy(),
	}

	autoTc.Spec.Cluster = &v1alpha1.TidbClusterRef{
		Namespace: tc.Namespace,
		Name:      tc.Name,
	}

	autoTc.Spec.TiCDC = nil
	autoTc.Spec.TiFlash = nil
	autoTc.Spec.PD = nil
	autoTc.Spec.Pump = nil
	t := true
	autoTc.Spec.EnablePVReclaim = &t

	switch component {
	case v1alpha1.TiDBMemberType.String():
		autoTc.Spec.TiKV = nil
		// Initialize Config
		if autoTc.Spec.TiDB.Config == nil {
			autoTc.Spec.TiDB.Config = v1alpha1.NewTiDBConfig()
		}
	case v1alpha1.TiKVMemberType.String():
		autoTc.Spec.TiDB = nil
		// Initialize Config
		if autoTc.Spec.TiKV.Config == nil {
			autoTc.Spec.TiKV.Config = v1alpha1.NewTiKVConfig()
		}
	}

	return autoTc
}
