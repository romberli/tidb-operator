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

package tidbcluster

import (
	"context"
	"fmt"
	_ "net/http/pprof"
	"time"

	"github.com/onsi/ginkgo"
	"github.com/pingcap/advanced-statefulset/client/apis/apps/v1/helper"
	asclientset "github.com/pingcap/advanced-statefulset/client/client/clientset/versioned"
	v1 "k8s.io/api/core/v1"
	apiextensionsclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	aggregatorclient "k8s.io/kube-aggregator/pkg/client/clientset_generated/clientset"
	"k8s.io/kubernetes/test/e2e/framework"
	"k8s.io/kubernetes/test/e2e/framework/log"
	e2esset "k8s.io/kubernetes/test/e2e/framework/statefulset"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/pingcap/tidb-operator/pkg/client/clientset/versioned"
	"github.com/pingcap/tidb-operator/pkg/controller"
	"github.com/pingcap/tidb-operator/pkg/label"
	"github.com/pingcap/tidb-operator/pkg/scheme"
	"github.com/pingcap/tidb-operator/tests"
	e2econfig "github.com/pingcap/tidb-operator/tests/e2e/config"
	e2eframework "github.com/pingcap/tidb-operator/tests/e2e/framework"
	utilimage "github.com/pingcap/tidb-operator/tests/e2e/util/image"
	utilpod "github.com/pingcap/tidb-operator/tests/e2e/util/pod"
	"github.com/pingcap/tidb-operator/tests/e2e/util/portforward"
	utilstatefulset "github.com/pingcap/tidb-operator/tests/e2e/util/statefulset"
	utiltc "github.com/pingcap/tidb-operator/tests/e2e/util/tidbcluster"
	"github.com/pingcap/tidb-operator/tests/pkg/fixture"
)

var _ = ginkgo.Describe("[Stability]", func() {
	f := e2eframework.NewDefaultFramework("asts")

	var ns string
	var c clientset.Interface
	var cli versioned.Interface
	var asCli asclientset.Interface
	var aggrCli aggregatorclient.Interface
	var apiExtCli apiextensionsclientset.Interface
	var hc clientset.Interface
	var cfg *tests.Config
	var config *restclient.Config
	var fw portforward.PortForward
	var fwCancel context.CancelFunc

	ginkgo.BeforeEach(func() {
		ns = f.Namespace.Name
		c = f.ClientSet
		var err error
		config, err = framework.LoadConfig()
		framework.ExpectNoError(err, "failed to load config")
		cli, err = versioned.NewForConfig(config)
		framework.ExpectNoError(err, "failed to create clientset")
		asCli, err = asclientset.NewForConfig(config)
		framework.ExpectNoError(err, "failed to create clientset")
		aggrCli, err = aggregatorclient.NewForConfig(config)
		framework.ExpectNoError(err, "failed to create clientset")
		apiExtCli, err = apiextensionsclientset.NewForConfig(config)
		framework.ExpectNoError(err, "failed to create clientset")
		clientRawConfig, err := e2econfig.LoadClientRawConfig()
		framework.ExpectNoError(err, "failed to load raw config")
		hc = helper.NewHijackClient(c, asCli)
		ctx, cancel := context.WithCancel(context.Background())
		fw, err = portforward.NewPortForwarder(ctx, e2econfig.NewSimpleRESTClientGetter(clientRawConfig))
		framework.ExpectNoError(err, "failed to create port forwarder")
		fwCancel = cancel
		cfg = e2econfig.TestConfig
	})

	ginkgo.AfterEach(func() {
		if fwCancel != nil {
			fwCancel()
		}
	})

	// tidb-operator with AdvancedStatefulSet feature enabled
	ginkgo.Context("[Feature: AdvancedStatefulSet][Feature: Webhook]", func() {
		var ocfg *tests.OperatorConfig
		var oa *tests.OperatorActions
		var genericCli client.Client

		ginkgo.BeforeEach(func() {
			ocfg = &tests.OperatorConfig{
				Namespace:      ns,
				ReleaseName:    "operator",
				Image:          cfg.OperatorImage,
				Tag:            cfg.OperatorTag,
				SchedulerImage: "k8s.gcr.io/kube-scheduler",
				Features: []string{
					"StableScheduling=true",
					"AdvancedStatefulSet=true",
				},
				LogLevel:          "4",
				ImagePullPolicy:   v1.PullIfNotPresent,
				TestMode:          true,
				WebhookEnabled:    true,
				PodWebhookEnabled: true,
				StsWebhookEnabled: false,
			}
			oa = tests.NewOperatorActions(cli, c, asCli, aggrCli, apiExtCli, tests.DefaultPollInterval, ocfg, e2econfig.TestConfig, nil, fw, f)
			ginkgo.By("Installing CRDs")
			oa.CleanCRDOrDie()
			oa.InstallCRDOrDie(ocfg)
			ginkgo.By("Installing tidb-operator")
			oa.CleanOperatorOrDie(ocfg)
			oa.DeployOperatorOrDie(ocfg)
			var err error
			genericCli, err = client.New(config, client.Options{Scheme: scheme.Scheme})
			framework.ExpectNoError(err, "failed to create clientset")
		})

		ginkgo.AfterEach(func() {
			ginkgo.By("Uninstall tidb-operator")
			oa.CleanOperatorOrDie(ocfg)
			ginkgo.By("Uninstalling CRDs")
			oa.CleanCRDOrDie()
		})

		ginkgo.It("Scaling tidb cluster with advanced statefulset", func() {
			clusterName := "scaling-with-asts"
			tc := fixture.GetTidbCluster(ns, clusterName, utilimage.TiDBLatest)
			tc = fixture.AddTiFlashForTidbCluster(tc)
			tc.Spec.PD.Replicas = 3
			tc.Spec.TiKV.Replicas = 5
			tc.Spec.TiDB.Replicas = 5
			tc.Spec.TiFlash.Replicas = 5
			utiltc.MustCreateTCWithComponentsReady(genericCli, oa, tc, 30*time.Minute, 15*time.Second)

			scalingTests := []struct {
				name        string
				component   string // tikv,pd,tidb,tiflash
				replicas    int32
				deleteSlots sets.Int32
			}{
				{
					name:        "Scaling in tikv from 5 to 3 by deleting pods 1 and 3",
					component:   "tikv",
					replicas:    3,
					deleteSlots: sets.NewInt32(1, 3),
				},
				{
					name:        "Scaling out tikv from 3 to 4 by adding pod 3",
					component:   "tikv",
					replicas:    4,
					deleteSlots: sets.NewInt32(1),
				},
				{
					name:        "Scaling tikv by adding pod 1 and deleting pod 2",
					component:   "tikv",
					replicas:    4,
					deleteSlots: sets.NewInt32(2),
				},
				{
					name:        "Scaling in tiflash from 5 to 3 by deleting pods 1 and 3",
					component:   "tiflash",
					replicas:    3,
					deleteSlots: sets.NewInt32(1, 3),
				},
				{
					name:        "Scaling out tiflash from 3 to 4 by adding pod 3",
					component:   "tiflash",
					replicas:    4,
					deleteSlots: sets.NewInt32(1),
				},
				{
					name:        "Scaling tiflash by adding pod 1 and deleting pod 2",
					component:   "tiflash",
					replicas:    4,
					deleteSlots: sets.NewInt32(2),
				},
				{
					name:        "Scaling in tidb from 3 to 2 by deleting pod 1",
					component:   "tidb",
					replicas:    2,
					deleteSlots: sets.NewInt32(1),
				},
				{
					name:        "Scaling in tidb from 2 to 0",
					component:   "tidb",
					replicas:    0,
					deleteSlots: sets.NewInt32(),
				},
				{
					name:        "Scaling out tidb from 0 to 2 by adding pods 0 and 2",
					component:   "tidb",
					replicas:    2,
					deleteSlots: sets.NewInt32(1),
				},
				{
					name:        "Scaling tidb from 2 to 4 by deleting pods 2 and adding pods 3, 4 and 5",
					component:   "tidb",
					replicas:    4,
					deleteSlots: sets.NewInt32(1, 2),
				},
				{
					name:        "Scaling out pd from 3 to 5 by adding pods 3, 4",
					component:   "pd",
					replicas:    5,
					deleteSlots: sets.NewInt32(),
				},
				{
					name:        "Scaling in pd from 5 to 3 by deleting pods 0 and 3",
					component:   "pd",
					replicas:    3,
					deleteSlots: sets.NewInt32(0, 3),
				},
				{
					name:        "Scaling out pd from 3 to 5 by adding pods 5 and 6",
					component:   "pd",
					replicas:    5,
					deleteSlots: sets.NewInt32(0, 3),
				},
			}

			for _, st := range scalingTests {
				ginkgo.By(st.name)
				replicas := st.replicas
				stsName := fmt.Sprintf("%s-%s", clusterName, st.component)

				sts, err := hc.AppsV1().StatefulSets(ns).Get(stsName, metav1.GetOptions{})
				framework.ExpectNoError(err, "failed to get statefulset %s/%s", ns, stsName)

				oldPodList := e2esset.GetPodList(c, sts)

				ginkgo.By(fmt.Sprintf("Scaling sts %s/%s to replicas %d and setting deleting pods to %v (old replicas: %d, old delete slots: %v)", ns, stsName, replicas, st.deleteSlots.List(), *sts.Spec.Replicas, helper.GetDeleteSlots(sts).List()))
				tc, err := cli.PingcapV1alpha1().TidbClusters(ns).Get(clusterName, metav1.GetOptions{})
				framework.ExpectNoError(err, "failed to get TidbCluster %s/%s", ns, clusterName)
				err = controller.GuaranteedUpdate(genericCli, tc, func() error {
					if tc.Annotations == nil {
						tc.Annotations = map[string]string{}
					}
					if st.component == "tikv" {
						tc.Annotations[label.AnnTiKVDeleteSlots] = mustToString(st.deleteSlots)
						tc.Spec.TiKV.Replicas = replicas
					} else if st.component == "tiflash" {
						tc.Annotations[label.AnnTiFlashDeleteSlots] = mustToString(st.deleteSlots)
						tc.Spec.TiFlash.Replicas = replicas
					} else if st.component == "pd" {
						tc.Annotations[label.AnnPDDeleteSlots] = mustToString(st.deleteSlots)
						tc.Spec.PD.Replicas = replicas
					} else if st.component == "tidb" {
						tc.Annotations[label.AnnTiDBDeleteSlots] = mustToString(st.deleteSlots)
						tc.Spec.TiDB.Replicas = replicas
					} else {
						return fmt.Errorf("unsupported component: %v", st.component)
					}
					return nil
				})
				framework.ExpectNoError(err, "failed to update TidbCluster %s/%s", ns, clusterName)

				ginkgo.By(fmt.Sprintf("Waiting for all pods of tidb cluster component %s (sts: %s/%s) are in desired state (replicas: %d, delete slots: %v)", st.component, ns, stsName, st.replicas, st.deleteSlots.List()))
				err = wait.PollImmediate(time.Second*5, time.Minute*15, func() (bool, error) {
					// check replicas and delete slots are synced
					sts, err = hc.AppsV1().StatefulSets(ns).Get(stsName, metav1.GetOptions{})
					if err != nil {
						return false, nil
					}
					if *sts.Spec.Replicas != st.replicas {
						log.Logf("replicas of sts %s/%s is %d, expects %d", ns, stsName, *sts.Spec.Replicas, st.replicas)
						return false, nil
					}
					if !helper.GetDeleteSlots(sts).Equal(st.deleteSlots) {
						log.Logf("delete slots of sts %s/%s is %v, expects %v", ns, stsName, helper.GetDeleteSlots(sts).List(), st.deleteSlots.List())
						return false, nil
					}
					// check all desired pods are running and ready
					return utilstatefulset.IsAllDesiredPodsRunningAndReady(hc, sts), nil
				})
				framework.ExpectNoError(err, "failed to wait for pod in TidbCluster component %s ready", st.component)

				ginkgo.By(fmt.Sprintf("Verify other pods of sts %s/%s should not be affected", ns, stsName))
				newPodList := e2esset.GetPodList(c, sts)
				framework.ExpectEqual(len(newPodList.Items), int(*sts.Spec.Replicas))
				for _, newPod := range newPodList.Items {
					for _, oldPod := range oldPodList.Items {
						// if the pod is not new or deleted in scaling, it should not be affected
						if oldPod.Name == newPod.Name && oldPod.UID != newPod.UID {
							framework.Failf("pod %s/%s should not be affected (UID: %s, OLD UID: %s)", newPod.Namespace, newPod.Name, newPod.UID, oldPod.UID)
						}
					}
				}
			}

			err := oa.WaitForTidbClusterReady(tc, 30*time.Minute, 15*time.Second)
			framework.ExpectNoError(err, "failed to wait for TidbCluster ready: %v", tc)
		})
	})

	ginkgo.It("[Feature: AdvancedStatefulSet] Upgrade to advanced statefulset", func() {
		var ocfg *tests.OperatorConfig
		var oa *tests.OperatorActions
		var genericCli client.Client

		ocfg = &tests.OperatorConfig{
			Namespace:      ns,
			ReleaseName:    "operator",
			Image:          cfg.OperatorImage,
			Tag:            cfg.OperatorTag,
			SchedulerImage: "k8s.gcr.io/kube-scheduler",
			Features: []string{
				"StableScheduling=true",
				"AdvancedStatefulSet=false",
			},
			LogLevel:        "4",
			ImagePullPolicy: v1.PullIfNotPresent,
			TestMode:        true,
		}
		oa = tests.NewOperatorActions(cli, c, asCli, aggrCli, apiExtCli, tests.DefaultPollInterval, ocfg, e2econfig.TestConfig, nil, fw, f)
		ginkgo.By("Installing CRDs")
		oa.CleanCRDOrDie()
		oa.InstallCRDOrDie(ocfg)
		ginkgo.By("Installing tidb-operator without AdvancedStatefulSet feature")
		oa.CleanOperatorOrDie(ocfg)
		oa.DeployOperatorOrDie(ocfg)
		var err error
		genericCli, err = client.New(config, client.Options{Scheme: scheme.Scheme})
		framework.ExpectNoError(err, "failed to create clientset")

		defer func() {
			ginkgo.By("Uninstall tidb-operator")
			oa.CleanOperatorOrDie(ocfg)
			ginkgo.By("Uninstalling CRDs")
			oa.CleanCRDOrDie()
		}()

		tc := fixture.GetTidbCluster(ns, "sts", utilimage.TiDBLatest)
		err = genericCli.Create(context.TODO(), tc)
		framework.ExpectNoError(err, "failed to create TidbCluster: %v", tc)
		err = oa.WaitForTidbClusterReady(tc, 30*time.Minute, 15*time.Second)
		framework.ExpectNoError(err, "failed to wait for TidbCluster ready: %v", tc)

		listOption := metav1.ListOptions{
			LabelSelector: labels.SelectorFromSet(map[string]string{
				label.InstanceLabelKey: tc.Name,
			}).String(),
		}
		stsList, err := c.AppsV1().StatefulSets(tc.Namespace).List(listOption)
		framework.ExpectNoError(err, "failed to list statefulsets in ns %s: %v", tc.Namespace, listOption)
		if len(stsList.Items) < 3 {
			log.Failf("at least 3 statefulsets must be created, got %d", len(stsList.Items))
		}

		podListBeforeUpgrade, err := c.CoreV1().Pods(tc.Namespace).List(listOption)
		framework.ExpectNoError(err, "failed to list pods in ns %s: %v", tc.Namespace, listOption)

		ginkgo.By("Upgrading tidb-operator with AdvancedStatefulSet feature")
		ocfg.Features = []string{
			"StableScheduling=true",
			"AdvancedStatefulSet=true",
		}
		oa.InstallCRDOrDie(ocfg)
		oa.UpgradeOperatorOrDie(ocfg)

		ginkgo.By("Wait for the advanced statefulsets are created and Kubernetes statfulsets are deleted")
		err = wait.PollImmediate(time.Second*5, time.Minute*5, func() (bool, error) {
			advancedStsList, err := asCli.AppsV1().StatefulSets(tc.Namespace).List(listOption)
			if err != nil {
				return false, nil
			}
			if len(advancedStsList.Items) != len(stsList.Items) {
				log.Logf("advanced statefulsets got %d, expect %d", len(advancedStsList.Items), len(stsList.Items))
				return false, nil
			}
			stsListAfterUpgrade, err := c.AppsV1().StatefulSets(tc.Namespace).List(listOption)
			if err != nil {
				return false, nil
			}
			if len(stsListAfterUpgrade.Items) != 0 {
				log.Logf("Kubernetes statefulsets got %d, expect %d", len(stsListAfterUpgrade.Items), 0)
				return false, nil
			}
			return true, nil
		})
		framework.ExpectNoError(err, "failed to wait for the advanced statefulsets are created and Kubernetes statfulsets are deleted")

		ginkgo.By("Make sure pods are not changed")
		err = utilpod.WaitForPodsAreChanged(c, podListBeforeUpgrade.Items, time.Minute*3)
		framework.ExpectEqual(err, wait.ErrWaitTimeout, "Pods are changed after the operator is upgraded")
	})

	ginkgo.It("[Feature: AdvancedStatefulSet] Upgrading tidb cluster while pods are not consecutive", func() {
		var ocfg *tests.OperatorConfig
		var oa *tests.OperatorActions
		var genericCli client.Client

		ocfg = &tests.OperatorConfig{
			Namespace:      ns,
			ReleaseName:    "operator",
			Image:          cfg.OperatorImage,
			Tag:            cfg.OperatorTag,
			SchedulerImage: "k8s.gcr.io/kube-scheduler",
			Features: []string{
				"StableScheduling=true",
				"AdvancedStatefulSet=false",
			},
			LogLevel:        "4",
			ImagePullPolicy: v1.PullIfNotPresent,
			TestMode:        true,
		}
		oa = tests.NewOperatorActions(cli, c, asCli, aggrCli, apiExtCli, tests.DefaultPollInterval, ocfg, e2econfig.TestConfig, nil, fw, f)
		ginkgo.By("Installing CRDs")
		oa.CleanCRDOrDie()
		oa.InstallCRDOrDie(ocfg)
		ginkgo.By("Installing tidb-operator without AdvancedStatefulSet feature")
		oa.CleanOperatorOrDie(ocfg)
		oa.DeployOperatorOrDie(ocfg)
		var err error
		genericCli, err = client.New(config, client.Options{Scheme: scheme.Scheme})
		framework.ExpectNoError(err, "failed to create clientset")

		defer func() {
			ginkgo.By("Uninstall tidb-operator")
			oa.CleanOperatorOrDie(ocfg)
			ginkgo.By("Uninstalling CRDs")
			oa.CleanCRDOrDie()
		}()

		tc := fixture.GetTidbCluster(ns, "upgrade-cluster", utilimage.TiDBLatestPrev)
		tc.Spec.PD.Replicas = 5
		tc.Spec.TiKV.Replicas = 4
		tc.Spec.TiDB.Replicas = 3
		err = genericCli.Create(context.TODO(), tc)
		framework.ExpectNoError(err, "failed to create TidbCluster: %v", tc)
		err = oa.WaitForTidbClusterReady(tc, 30*time.Minute, 15*time.Second)
		framework.ExpectNoError(err, "failed to wait for TidbCluster ready: %v", tc)

		ginkgo.By("Scaling in the cluster by deleting some pods not at the end")
		tc, err = cli.PingcapV1alpha1().TidbClusters(ns).Get(tc.Name, metav1.GetOptions{})
		framework.ExpectNoError(err, "failed to get TidbCluster %s/%s", ns, tc.Name)
		err = controller.GuaranteedUpdate(genericCli, tc, func() error {
			if tc.Annotations == nil {
				tc.Annotations = map[string]string{}
			}
			tc.Annotations[label.AnnPDDeleteSlots] = "[1]"
			tc.Annotations[label.AnnTiKVDeleteSlots] = "[0]"
			tc.Annotations[label.AnnTiDBDeleteSlots] = "[1]"
			tc.Spec.PD.Replicas = 3
			tc.Spec.TiKV.Replicas = 3
			tc.Spec.TiDB.Replicas = 2
			return nil
		})
		framework.ExpectNoError(err, "failed to update TidbCluster %s/%s", ns, tc.Name)
		ginkgo.By("Checking for tidb cluster is ready")
		err = oa.WaitForTidbClusterReady(tc, 30*time.Minute, 15*time.Second)
		framework.ExpectNoError(err, "failed to wait for TidbCluster ready: %v", tc)

		ginkgo.By("Upgrading the cluster")
		err = controller.GuaranteedUpdate(genericCli, tc, func() error {
			tc.Spec.Version = utilimage.TiDBLatest
			return nil
		})
		framework.ExpectNoError(err, "failed to upgrade TidbCluster %s/%s", ns, tc.Name)
		ginkgo.By("Checking for tidb cluster is ready")
		err = oa.WaitForTidbClusterReady(tc, 30*time.Minute, 15*time.Second)
		framework.ExpectNoError(err, "failed to wait for TidbCluster ready: %v", tc)
	})
})
