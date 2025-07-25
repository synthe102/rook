/*
Copyright 2018 The Rook Authors. All rights reserved.

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

package mon

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	cephv1 "github.com/rook/rook/pkg/apis/ceph.rook.io/v1"
	"github.com/rook/rook/pkg/clusterd"
	cephclient "github.com/rook/rook/pkg/daemon/ceph/client"
	clienttest "github.com/rook/rook/pkg/daemon/ceph/client/test"
	"github.com/rook/rook/pkg/operator/ceph/config"
	opcontroller "github.com/rook/rook/pkg/operator/ceph/controller"
	testopk8s "github.com/rook/rook/pkg/operator/k8sutil/test"
	"github.com/rook/rook/pkg/operator/test"
	exectest "github.com/rook/rook/pkg/util/exec/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apps "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

func TestCheckHealth(t *testing.T) {
	ctx := context.TODO()
	var deploymentsUpdated *[]*apps.Deployment
	updateDeploymentAndWait, deploymentsUpdated = testopk8s.UpdateDeploymentAndWaitStub()

	executor := &exectest.MockExecutor{
		MockExecuteCommandWithOutput: func(command string, args ...string) (string, error) {
			logger.Infof("executing command: %s %+v", command, args)
			if args[0] == "auth" && args[1] == "get-or-create-key" {
				return "{\"key\":\"mysecurekey\"}", nil
			}
			return clienttest.MonInQuorumResponse(), nil
		},
	}
	clientset := test.New(t, 1)
	configDir := t.TempDir()
	context := &clusterd.Context{
		Clientset: clientset,
		ConfigDir: configDir,
		Executor:  executor,
	}
	ownerInfo := cephclient.NewMinimumOwnerInfoWithOwnerRef()
	c := New(ctx, context, "ns", cephv1.ClusterSpec{}, ownerInfo)
	// clusterInfo is nil so we return err
	err := c.checkHealth(ctx)
	assert.NotNil(t, err)

	setCommonMonProperties(c, 1, cephv1.MonSpec{Count: 0, AllowMultiplePerNode: true}, "myversion")
	// mon count is 0 so we return err
	err = c.checkHealth(ctx)
	assert.NotNil(t, err)

	c.spec.Mon.Count = 3
	logger.Infof("initial mons: %v", c.ClusterInfo.InternalMonitors)
	c.waitForStart = false

	c.mapping.Schedule["f"] = &opcontroller.MonScheduleInfo{
		Name:    "node0",
		Address: "",
	}
	c.maxMonID = 4

	// mock out the scheduler to return node0
	waitForMonitorScheduling = func(c *Cluster, d *apps.Deployment) (SchedulingResult, error) {
		node, _ := clientset.CoreV1().Nodes().Get(ctx, "node0", metav1.GetOptions{})
		return SchedulingResult{Node: node}, nil
	}

	c.ClusterInfo.Context = ctx
	err = c.checkHealth(ctx)
	assert.Nil(t, err)
	logger.Infof("mons after checkHealth: %v", c.ClusterInfo.InternalMonitors)
	assert.ElementsMatch(t, []string{"rook-ceph-mon-a", "rook-ceph-mon-f"}, testopk8s.DeploymentNamesUpdated(deploymentsUpdated))
	testopk8s.ClearDeploymentsUpdated(deploymentsUpdated)

	err = c.failoverMon("f")
	assert.Nil(t, err)
	assert.ElementsMatch(t, []string{}, testopk8s.DeploymentNamesUpdated(deploymentsUpdated))
	testopk8s.ClearDeploymentsUpdated(deploymentsUpdated)

	newMons := []string{
		"g",
	}
	for _, monName := range newMons {
		_, ok := c.ClusterInfo.InternalMonitors[monName]
		assert.True(t, ok, fmt.Sprintf("mon %s not found in monitor list. %v", monName, c.ClusterInfo.InternalMonitors))
	}

	deployments, err := clientset.AppsV1().Deployments(c.Namespace).List(ctx, metav1.ListOptions{})
	assert.NoError(t, err)
	assert.Equal(t, 3, len(deployments.Items))

	// no orphan resources to remove
	c.removeOrphanMonResources()

	// We expect mons to exist: a, g, h
	// Check that their PVCs are not garbage collected after we create fake PVCs
	badMon := "c"
	goodMons := []string{"a", "g", "h"}
	c.spec.Mon.VolumeClaimTemplate = &cephv1.VolumeClaimTemplate{}
	for _, name := range append(goodMons, badMon) {
		m := &monConfig{ResourceName: "rook-ceph-mon-" + name, DaemonName: name}
		pvc, err := c.makeDeploymentPVC(m, true)
		assert.NoError(t, err)
		_, err = c.context.Clientset.CoreV1().PersistentVolumeClaims(c.Namespace).Create(ctx, pvc, metav1.CreateOptions{})
		assert.NoError(t, err)
	}

	pvcs, err := c.context.Clientset.CoreV1().PersistentVolumeClaims(c.Namespace).List(ctx, metav1.ListOptions{})
	assert.NoError(t, err)
	assert.Equal(t, 4, len(pvcs.Items))

	// pvc "c" should be removed and the others should remain
	c.removeOrphanMonResources()
	pvcs, err = c.context.Clientset.CoreV1().PersistentVolumeClaims(c.Namespace).List(ctx, metav1.ListOptions{})
	assert.NoError(t, err)
	assert.Equal(t, 3, len(pvcs.Items))
	for _, pvc := range pvcs.Items {
		found := false
		for _, name := range goodMons {
			if pvc.Name == "rook-ceph-mon-"+name {
				found = true
				break
			}
		}
		assert.True(t, found, pvc.Name)
	}
}

func TestRemoveExtraMon(t *testing.T) {
	endpoint := "1.2.3.4:6789"
	c := &Cluster{mapping: &opcontroller.Mapping{}}
	c.ClusterInfo = &cephclient.ClusterInfo{InternalMonitors: map[string]*cephclient.MonInfo{
		"a": {Name: "a", Endpoint: endpoint},
		"b": {Name: "b", Endpoint: endpoint},
		"c": {Name: "c", Endpoint: endpoint},
		"d": {Name: "d", Endpoint: endpoint},
	}}
	c.mapping.Schedule = map[string]*opcontroller.MonScheduleInfo{
		"a": {Name: "node1"},
		"b": {Name: "node2"},
		"c": {Name: "node3"},
		"d": {Name: "node1"},
	}
	// Remove mon an extra mon on the same node
	removedMon := c.determineExtraMonToRemove()
	if removedMon != "a" && removedMon != "d" {
		assert.Fail(t, fmt.Sprintf("removed mon %q instead of a or d", removedMon))
	}

	// Remove an arbitrary mon that are all on different nodes
	c.mapping.Schedule["d"].Name = "node4"
	removedMon = c.determineExtraMonToRemove()
	assert.NotEqual(t, "", removedMon)

	// Don't remove any extra mon from a proper stretch cluster
	c.spec.Mon.StretchCluster = &cephv1.StretchClusterSpec{Zones: []cephv1.MonZoneSpec{
		{Name: "x", Arbiter: true},
		{Name: "y"},
		{Name: "z"},
	}}
	c.ClusterInfo.InternalMonitors["e"] = &cephclient.MonInfo{Name: "e", Endpoint: endpoint}
	c.mapping.Schedule["a"].Zone = "x"
	c.mapping.Schedule["b"].Zone = "y"
	c.mapping.Schedule["c"].Zone = "y"
	c.mapping.Schedule["d"].Zone = "z"
	c.mapping.Schedule["e"] = &opcontroller.MonScheduleInfo{Name: "node5", Zone: "z"}
	removedMon = c.determineExtraMonToRemove()
	assert.Equal(t, "", removedMon)

	// Remove an extra mon from the arbiter zone
	c.mapping.Schedule["d"].Zone = "x"
	removedMon = c.determineExtraMonToRemove()
	if removedMon != "a" && removedMon != "d" {
		assert.Fail(t, "removed mon %q instead of a or d from the arbiter zone", removedMon)
	}

	// Remove an extra mon from a non-arbiter zone
	c.mapping.Schedule["d"].Zone = "y"
	removedMon = c.determineExtraMonToRemove()
	if removedMon != "b" && removedMon != "c" && removedMon != "d" {
		assert.Fail(t, fmt.Sprintf("removed mon %q instead of b, c, or d from the non-arbiter zone", removedMon))
	}
}

func TestTrackMonsOutOfQuorum(t *testing.T) {
	endpoint := "1.2.3.4:6789"
	clientset := test.New(t, 1)
	tempDir, err := os.MkdirTemp("", "")
	assert.NoError(t, err)
	defer os.RemoveAll(tempDir)
	ownerInfo := cephclient.NewMinimumOwnerInfoWithOwnerRef()
	c := &Cluster{
		mapping:   &opcontroller.Mapping{},
		context:   &clusterd.Context{Clientset: clientset, ConfigDir: tempDir},
		ownerInfo: ownerInfo,
		Namespace: "ns",
	}
	c.ClusterInfo = &cephclient.ClusterInfo{InternalMonitors: map[string]*cephclient.MonInfo{
		"a": {Name: "a", Endpoint: endpoint},
		"b": {Name: "b", Endpoint: endpoint},
		"c": {Name: "c", Endpoint: endpoint},
	}}
	// No change since all mons are in quorum
	updated, err := c.trackMonInOrOutOfQuorum("a", true)
	assert.False(t, updated)
	assert.NoError(t, err)

	// initialize the configmap
	err = c.persistExpectedMonDaemonsInConfigMap()
	assert.NoError(t, err)

	// Track mon.a as out of quorum
	updated, err = c.trackMonInOrOutOfQuorum("a", false)
	assert.True(t, updated)
	assert.NoError(t, err)

	cm, err := clientset.CoreV1().ConfigMaps(c.Namespace).Get(context.TODO(), EndpointConfigMapName, metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Equal(t, "a", cm.Data[opcontroller.OutOfQuorumKey])

	// Put mon.a back in quorum
	updated, err = c.trackMonInOrOutOfQuorum("a", true)
	assert.True(t, updated)
	assert.NoError(t, err)

	cm, err = clientset.CoreV1().ConfigMaps(c.Namespace).Get(context.TODO(), EndpointConfigMapName, metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Equal(t, "", cm.Data[opcontroller.OutOfQuorumKey])
}

func TestEvictMonOnSameNode(t *testing.T) {
	ctx := context.TODO()
	clientset := test.New(t, 1)
	configDir := t.TempDir()
	executor := &exectest.MockExecutor{
		MockExecuteCommandWithOutput: func(command string, args ...string) (string, error) {
			logger.Infof("executing command: %s %+v", command, args)
			return "{\"key\":\"mysecurekey\"}", nil
		},
	}
	context := &clusterd.Context{Clientset: clientset, ConfigDir: configDir, Executor: executor}
	ownerInfo := cephclient.NewMinimumOwnerInfoWithOwnerRef()
	c := New(ctx, context, "ns", cephv1.ClusterSpec{}, ownerInfo)
	setCommonMonProperties(c, 1, cephv1.MonSpec{Count: 0}, "myversion")
	c.maxMonID = 2
	c.waitForStart = false
	waitForMonitorScheduling = func(c *Cluster, d *apps.Deployment) (SchedulingResult, error) {
		node, _ := clientset.CoreV1().Nodes().Get(ctx, "node0", metav1.GetOptions{})
		return SchedulingResult{Node: node}, nil
	}

	c.spec.Mon.Count = 3
	createTestMonPod(t, clientset, c, "a", "node1")

	// Nothing to evict with a single mon
	err := c.evictMonIfMultipleOnSameNode()
	assert.NoError(t, err)

	// Create a second mon on a different node
	createTestMonPod(t, clientset, c, "b", "node2")

	// Nothing to evict with where mons are on different nodes
	err = c.evictMonIfMultipleOnSameNode()
	assert.NoError(t, err)

	// Create a third mon on the same node as mon a
	createTestMonPod(t, clientset, c, "c", "node1")
	assert.Equal(t, 2, c.maxMonID)

	// Should evict either mon a or mon c since they are on the same node and failover to mon d
	c.ClusterInfo.Context = ctx
	err = c.evictMonIfMultipleOnSameNode()
	assert.NoError(t, err)
	_, err = clientset.AppsV1().Deployments(c.Namespace).Get(ctx, "rook-ceph-mon-d", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Equal(t, 3, c.maxMonID)
}

func TestHostNetworkFailover(t *testing.T) {
	ctx := context.TODO()
	context := &clusterd.Context{}
	ownerInfo := cephclient.NewMinimumOwnerInfoWithOwnerRef()
	c := New(ctx, context, "ns", cephv1.ClusterSpec{}, ownerInfo)

	t.Run("should stop mon on default network", func(t *testing.T) {
		assert.True(t, c.stopMonDuringFailover("a"))
	})

	t.Run("should not stop mon on host network", func(t *testing.T) {
		c.spec.Network.Provider = "host"
		assert.False(t, c.stopMonDuringFailover("a"))
	})

	t.Run("should stop mon converting to host network", func(t *testing.T) {
		c.spec.Network.Provider = "host"
		c.monsToFailover["a"] = &monConfig{UseHostNetwork: false}
		assert.True(t, c.stopMonDuringFailover("a"))
	})

	t.Run("should stop mon converting from host network", func(t *testing.T) {
		c.spec.Network.Provider = ""
		c.monsToFailover["a"] = &monConfig{UseHostNetwork: true}
		assert.True(t, c.stopMonDuringFailover("a"))
	})
}

func createTestMonPod(t *testing.T, clientset kubernetes.Interface, c *Cluster, name, node string) {
	m := &monConfig{ResourceName: resourceName(name), DaemonName: name, DataPathMap: &config.DataPathMap{}}
	d, err := c.makeDeployment(m, false)
	assert.NoError(t, err)
	monPod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "mon-pod-" + name, Namespace: c.Namespace, Labels: d.Labels},
		Spec:       d.Spec.Template.Spec,
	}
	monPod.Spec.NodeName = node
	monPod.Status.Phase = v1.PodRunning
	_, err = clientset.CoreV1().Pods(c.Namespace).Create(context.TODO(), monPod, metav1.CreateOptions{})
	assert.NoError(t, err)
}

func TestScaleMonDeployment(t *testing.T) {
	ctx := context.TODO()
	clientset := test.New(t, 1)
	context := &clusterd.Context{Clientset: clientset}
	ownerInfo := cephclient.NewMinimumOwnerInfoWithOwnerRef()
	c := New(ctx, context, "ns", cephv1.ClusterSpec{}, ownerInfo)
	setCommonMonProperties(c, 1, cephv1.MonSpec{Count: 0, AllowMultiplePerNode: true}, "myversion")

	name := "a"
	c.spec.Mon.Count = 3
	logger.Infof("initial mons: %v", c.ClusterInfo.InternalMonitors[name])
	monConfig := &monConfig{ResourceName: resourceName(name), DaemonName: name, DataPathMap: &config.DataPathMap{}}
	d, err := c.makeDeployment(monConfig, false)
	require.NoError(t, err)
	_, err = clientset.AppsV1().Deployments(c.Namespace).Create(ctx, d, metav1.CreateOptions{})
	require.NoError(t, err)

	verifyMonReplicas(ctx, t, c, name, 1)
	err = c.updateMonDeploymentReplica(name, false)
	assert.NoError(t, err)
	verifyMonReplicas(ctx, t, c, name, 0)

	err = c.updateMonDeploymentReplica(name, true)
	assert.NoError(t, err)
	verifyMonReplicas(ctx, t, c, name, 1)
}

func verifyMonReplicas(ctx context.Context, t *testing.T, c *Cluster, name string, expected int32) {
	d, err := c.context.Clientset.AppsV1().Deployments(c.Namespace).Get(ctx, resourceName("a"), metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, expected, *d.Spec.Replicas)
}

func TestCheckHealthNotFound(t *testing.T) {
	ctx := context.TODO()
	var deploymentsUpdated *[]*apps.Deployment
	updateDeploymentAndWait, deploymentsUpdated = testopk8s.UpdateDeploymentAndWaitStub()

	executor := &exectest.MockExecutor{
		MockExecuteCommandWithOutput: func(command string, args ...string) (string, error) {
			logger.Infof("executing command: %s %+v", command, args)
			if args[0] == "auth" && args[1] == "get-or-create-key" {
				return "{\"key\":\"mysecurekey\"}", nil
			}
			return clienttest.MonInQuorumResponse(), nil
		},
	}
	clientset := test.New(t, 1)
	configDir := t.TempDir()
	context := &clusterd.Context{
		Clientset: clientset,
		ConfigDir: configDir,
		Executor:  executor,
	}
	ownerInfo := cephclient.NewMinimumOwnerInfoWithOwnerRef()
	c := New(ctx, context, "ns", cephv1.ClusterSpec{}, ownerInfo)
	setCommonMonProperties(c, 2, cephv1.MonSpec{Count: 3, AllowMultiplePerNode: true}, "myversion")
	c.waitForStart = false

	c.mapping.Schedule["a"] = &opcontroller.MonScheduleInfo{
		Name: "node0",
	}
	c.mapping.Schedule["b"] = &opcontroller.MonScheduleInfo{
		Name: "node0",
	}
	c.maxMonID = 4

	err := c.saveMonConfig()
	assert.NoError(t, err)

	// Check if the two mons are found in the configmap
	cm, err := c.context.Clientset.CoreV1().ConfigMaps(c.Namespace).Get(ctx, EndpointConfigMapName, metav1.GetOptions{})
	assert.Nil(t, err)
	if cm.Data[EndpointDataKey] != "a=1.2.3.1:3300,b=1.2.3.2:3300" {
		assert.Equal(t, "b=1.2.3.2:3300,a=1.2.3.1:3300", cm.Data[EndpointDataKey])
	}

	// Because the mon a isn't in the MonInQuorumResponse() this will create a new mon
	delete(c.mapping.Schedule, "b")
	err = c.checkHealth(ctx)
	assert.Nil(t, err)
	// No updates in unit tests w/ workaround
	assert.ElementsMatch(t, []string{}, testopk8s.DeploymentNamesUpdated(deploymentsUpdated))
	testopk8s.ClearDeploymentsUpdated(deploymentsUpdated)

	// recheck that the "not found" mon has been replaced with a new one
	cm, err = c.context.Clientset.CoreV1().ConfigMaps(c.Namespace).Get(ctx, EndpointConfigMapName, metav1.GetOptions{})
	assert.Nil(t, err)
	if cm.Data[EndpointDataKey] != "a=1.2.3.1:3300,f=:6789" {
		assert.Equal(t, "f=:6789,a=1.2.3.1:3300", cm.Data[EndpointDataKey])
	}
}

func TestAddRemoveMons(t *testing.T) {
	ctx := context.TODO()
	var deploymentsUpdated *[]*apps.Deployment
	updateDeploymentAndWait, deploymentsUpdated = testopk8s.UpdateDeploymentAndWaitStub()

	monQuorumResponse := clienttest.MonInQuorumResponse()
	executor := &exectest.MockExecutor{
		MockExecuteCommandWithOutput: func(command string, args ...string) (string, error) {
			logger.Infof("executing command: %s %+v", command, args)
			if args[0] == "auth" && args[1] == "get-or-create-key" {
				return "{\"key\":\"mysecurekey\"}", nil
			}
			return monQuorumResponse, nil
		},
	}
	clientset := test.New(t, 1)
	configDir := t.TempDir()
	context := &clusterd.Context{
		Clientset: clientset,
		ConfigDir: configDir,
		Executor:  executor,
	}
	ownerInfo := cephclient.NewMinimumOwnerInfoWithOwnerRef()
	c := New(ctx, context, "ns", cephv1.ClusterSpec{}, ownerInfo)
	setCommonMonProperties(c, 0, cephv1.MonSpec{Count: 5, AllowMultiplePerNode: true}, "myversion")
	c.maxMonID = 0 // "a" is max mon id
	c.waitForStart = false

	// checking the health will increase the mons as desired all in one go
	err := c.checkHealth(ctx)
	assert.Nil(t, err)
	assert.Equal(t, 5, len(c.ClusterInfo.InternalMonitors), fmt.Sprintf("mons: %v", c.ClusterInfo.InternalMonitors))
	assert.ElementsMatch(t, []string{
		// b is created first, no updates
		"rook-ceph-mon-b",                    // b updated when c created
		"rook-ceph-mon-b", "rook-ceph-mon-c", // b and c updated when d created
		"rook-ceph-mon-b", "rook-ceph-mon-c", "rook-ceph-mon-d", // etc.
		"rook-ceph-mon-b", "rook-ceph-mon-c", "rook-ceph-mon-d", "rook-ceph-mon-e",
	},
		testopk8s.DeploymentNamesUpdated(deploymentsUpdated))
	testopk8s.ClearDeploymentsUpdated(deploymentsUpdated)

	// reducing the mon count to 3 will reduce the mon count once each time we call checkHealth
	monQuorumResponse = clienttest.MonInQuorumResponseFromMons(c.ClusterInfo.InternalMonitors)
	c.spec.Mon.Count = 3
	err = c.checkHealth(ctx)
	assert.Nil(t, err)
	assert.Equal(t, 4, len(c.ClusterInfo.InternalMonitors))
	// No updates in unit tests w/ workaround
	assert.ElementsMatch(t, []string{}, testopk8s.DeploymentNamesUpdated(deploymentsUpdated))
	testopk8s.ClearDeploymentsUpdated(deploymentsUpdated)

	// after the second call we will be down to the expected count of 3
	monQuorumResponse = clienttest.MonInQuorumResponseFromMons(c.ClusterInfo.InternalMonitors)
	err = c.checkHealth(ctx)
	assert.Nil(t, err)
	assert.Equal(t, 3, len(c.ClusterInfo.InternalMonitors))
	// No updates in unit tests w/ workaround
	assert.ElementsMatch(t, []string{}, testopk8s.DeploymentNamesUpdated(deploymentsUpdated))
	testopk8s.ClearDeploymentsUpdated(deploymentsUpdated)

	// now attempt to reduce the mons down to quorum size 1
	monQuorumResponse = clienttest.MonInQuorumResponseFromMons(c.ClusterInfo.InternalMonitors)
	c.spec.Mon.Count = 1
	err = c.checkHealth(ctx)
	assert.Nil(t, err)
	assert.Equal(t, 2, len(c.ClusterInfo.InternalMonitors))
	// No updates in unit tests w/ workaround
	assert.ElementsMatch(t, []string{}, testopk8s.DeploymentNamesUpdated(deploymentsUpdated))
	testopk8s.ClearDeploymentsUpdated(deploymentsUpdated)

	// cannot reduce from quorum size of 2 to 1
	monQuorumResponse = clienttest.MonInQuorumResponseFromMons(c.ClusterInfo.InternalMonitors)
	err = c.checkHealth(ctx)
	assert.Nil(t, err)
	assert.Equal(t, 2, len(c.ClusterInfo.InternalMonitors))
	// No updates in unit tests w/ workaround
	assert.ElementsMatch(t, []string{}, testopk8s.DeploymentNamesUpdated(deploymentsUpdated))
	testopk8s.ClearDeploymentsUpdated(deploymentsUpdated)
}

func TestAddOrRemoveExternalMonitor(t *testing.T) {
	var changed bool
	var err error

	// populate fake monmap
	fakeResp := cephclient.MonStatusResponse{Quorum: []int{0}}

	fakeResp.MonMap.Mons = []cephclient.MonMapEntry{
		{
			Name: "a",
		},
	}
	fakeResp.MonMap.Mons[0].PublicAddr = "172.17.0.4:3300"

	// populate fake ClusterInfo
	c := &Cluster{ClusterInfo: &cephclient.ClusterInfo{}}
	c.ClusterInfo = clienttest.CreateTestClusterInfo(1)

	//
	// TEST 1
	//
	// both clusterInfo and mon map are identical so nil is expected
	changed, err = c.addOrRemoveExternalMonitor(fakeResp)
	assert.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, 1, len(c.ClusterInfo.InternalMonitors))

	//
	// TEST 2
	//
	// Now let's test the case where mon disappeared from the external cluster
	// ClusterInfo still has them but they are gone from the monmap.
	// Thus they should be removed from ClusterInfo
	c.ClusterInfo = clienttest.CreateTestClusterInfo(3)
	changed, err = c.addOrRemoveExternalMonitor(fakeResp)
	assert.NoError(t, err)
	assert.True(t, changed)
	// ClusterInfo should shrink to 1
	assert.Equal(t, 1, len(c.ClusterInfo.InternalMonitors))

	//
	// TEST 3
	//
	// Now let's add a new mon in the external cluster
	// ClusterInfo should be updated with this new monitor
	fakeResp.MonMap.Mons = []cephclient.MonMapEntry{
		{
			Name: "a",
		},
		{
			Name: "b",
		},
	}
	fakeResp.MonMap.Mons[1].PublicAddr = "172.17.0.5:3300"
	c.ClusterInfo = clienttest.CreateTestClusterInfo(1)
	changed, err = c.addOrRemoveExternalMonitor(fakeResp)
	assert.NoError(t, err)
	assert.True(t, changed)
	// ClusterInfo should now have 2 monitors
	assert.Equal(t, 2, len(c.ClusterInfo.InternalMonitors))
}

func TestNewHealthChecker(t *testing.T) {
	c := &Cluster{spec: cephv1.ClusterSpec{HealthCheck: cephv1.CephClusterHealthCheckSpec{}}}

	type args struct {
		monCluster *Cluster
	}
	tests := struct {
		name string
		args args
		want *HealthChecker
	}{
		"default-interval", args{c}, &HealthChecker{c, HealthCheckInterval},
	}
	t.Run(tests.name, func(t *testing.T) {
		if got := NewHealthChecker(tests.args.monCluster); !reflect.DeepEqual(got, tests.want) {
			t.Errorf("NewHealthChecker() = %v, want %v", got, tests.want)
		}
	})
}

func TestUpdateMonTimeout(t *testing.T) {
	t.Run("using default mon timeout", func(t *testing.T) {
		m := &Cluster{}
		updateMonTimeout(m)
		assert.Equal(t, time.Minute*10, MonOutTimeout)
	})
	t.Run("using env var mon timeout", func(t *testing.T) {
		t.Setenv("ROOK_MON_OUT_TIMEOUT", "10s")
		m := &Cluster{}
		updateMonTimeout(m)
		assert.Equal(t, time.Second*10, MonOutTimeout)
	})
	t.Run("using spec mon timeout", func(t *testing.T) {
		m := &Cluster{spec: cephv1.ClusterSpec{HealthCheck: cephv1.CephClusterHealthCheckSpec{DaemonHealth: cephv1.DaemonHealthSpec{Monitor: cephv1.HealthCheckSpec{Timeout: "1m"}}}}}
		updateMonTimeout(m)
		assert.Equal(t, time.Minute, MonOutTimeout)
	})
}

func TestUpdateMonInterval(t *testing.T) {
	t.Run("using default mon interval", func(t *testing.T) {
		m := &Cluster{}
		h := &HealthChecker{m, HealthCheckInterval}
		updateMonInterval(m, h)
		assert.Equal(t, time.Second*45, HealthCheckInterval)
	})
	t.Run("using env var mon timeout", func(t *testing.T) {
		t.Setenv("ROOK_MON_HEALTHCHECK_INTERVAL", "10s")
		m := &Cluster{}
		h := &HealthChecker{m, HealthCheckInterval}
		updateMonInterval(m, h)
		assert.Equal(t, time.Second*10, h.interval)
	})
	t.Run("using spec mon timeout", func(t *testing.T) {
		tm, err := time.ParseDuration("1m")
		assert.NoError(t, err)
		m := &Cluster{spec: cephv1.ClusterSpec{HealthCheck: cephv1.CephClusterHealthCheckSpec{DaemonHealth: cephv1.DaemonHealthSpec{Monitor: cephv1.HealthCheckSpec{Interval: &metav1.Duration{Duration: tm}}}}}}
		h := &HealthChecker{m, HealthCheckInterval}
		updateMonInterval(m, h)
		assert.Equal(t, time.Minute, h.interval)
	})
}

func Test_removeMonsFromQuorumStatusResponse(t *testing.T) {
	type args struct {
		quorumStatus cephclient.MonStatusResponse
		idsToRemove  []string
	}
	tests := []struct {
		name string
		args args
		want cephclient.MonStatusResponse
	}{
		{
			name: "remove one mon",
			args: args{
				quorumStatus: cephclient.MonStatusResponse{
					Quorum: []int{0, 1, 2},
					MonMap: struct {
						Mons []cephclient.MonMapEntry `json:"mons"`
					}{
						Mons: []cephclient.MonMapEntry{
							{
								Name: "a",
								Rank: 0,
							},
							{
								Name: "b",
								Rank: 1,
							},
							{
								Name: "c",
								Rank: 2,
							},
						},
					},
				},
				idsToRemove: []string{"b"},
			},
			want: cephclient.MonStatusResponse{
				Quorum: []int{0, 2},
				MonMap: struct {
					Mons []cephclient.MonMapEntry `json:"mons"`
				}{
					Mons: []cephclient.MonMapEntry{
						{
							Name: "a",
							Rank: 0,
						},
						{
							Name: "c",
							Rank: 2,
						},
					},
				},
			},
		},
		{
			name: "not remove if not present",
			args: args{
				quorumStatus: cephclient.MonStatusResponse{
					Quorum: []int{0, 1, 2},
					MonMap: struct {
						Mons []cephclient.MonMapEntry `json:"mons"`
					}{
						Mons: []cephclient.MonMapEntry{
							{
								Name: "a",
								Rank: 0,
							},
							{
								Name: "b",
								Rank: 1,
							},
							{
								Name: "c",
								Rank: 2,
							},
						},
					},
				},
				idsToRemove: []string{"e"},
			},
			want: cephclient.MonStatusResponse{
				Quorum: []int{0, 1, 2},
				MonMap: struct {
					Mons []cephclient.MonMapEntry `json:"mons"`
				}{
					Mons: []cephclient.MonMapEntry{
						{
							Name: "a",
							Rank: 0,
						},
						{
							Name: "b",
							Rank: 1,
						},
						{
							Name: "c",
							Rank: 2,
						},
					},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := removeMonsFromQuorumStatusResponse(tt.args.quorumStatus, tt.args.idsToRemove); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("removeMonsFromQuorumStatusResponse() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestExternalMons_notInSpec_InQuorum(t *testing.T) {
	// 1. setup test
	ctx := context.TODO()
	var deploymentsUpdated *[]*apps.Deployment
	updateDeploymentAndWait, deploymentsUpdated = testopk8s.UpdateDeploymentAndWaitStub()

	monQuorumResponse := clienttest.MonInQuorumResponse()
	executor := &exectest.MockExecutor{
		MockExecuteCommandWithOutput: func(command string, args ...string) (string, error) {
			logger.Infof("executing command: %s %+v", command, args)
			if args[0] == "auth" && args[1] == "get-or-create-key" {
				return "{\"key\":\"mysecurekey\"}", nil
			}
			return monQuorumResponse, nil
		},
	}
	clientset := test.New(t, 1)
	configDir := t.TempDir()
	context := &clusterd.Context{
		Clientset: clientset,
		ConfigDir: configDir,
		Executor:  executor,
	}
	ownerInfo := cephclient.NewMinimumOwnerInfoWithOwnerRef()
	c := New(ctx, context, "ns", cephv1.ClusterSpec{}, ownerInfo)
	setCommonMonProperties(c, 0, cephv1.MonSpec{Count: 5, AllowMultiplePerNode: true}, "myversion")
	c.maxMonID = 0 // "a" is max mon id
	c.waitForStart = false

	// checking the health will increase the mons as desired all in one go
	err := c.checkHealth(ctx)
	assert.Nil(t, err)
	assert.Equal(t, 5, len(c.ClusterInfo.InternalMonitors), fmt.Sprintf("mons: %v", c.ClusterInfo.InternalMonitors))
	assert.ElementsMatch(t, []string{
		// b is created first, no updates
		"rook-ceph-mon-b",                    // b updated when c created
		"rook-ceph-mon-b", "rook-ceph-mon-c", // b and c updated when d created
		"rook-ceph-mon-b", "rook-ceph-mon-c", "rook-ceph-mon-d", // etc.
		"rook-ceph-mon-b", "rook-ceph-mon-c", "rook-ceph-mon-d", "rook-ceph-mon-e",
	},
		testopk8s.DeploymentNamesUpdated(deploymentsUpdated))
	testopk8s.ClearDeploymentsUpdated(deploymentsUpdated)

	inital5Mons := make(map[string]*cephclient.MonInfo)
	for k, v := range c.ClusterInfo.InternalMonitors {
		inital5Mons[k] = v
	}

	// 2. add external mon to quorum but not in spec:

	mons := make(map[string]*cephclient.MonInfo)
	for k, v := range inital5Mons {
		mons[k] = v
	}
	// add unknown mon to quorum:
	mons["ext-mon-id"] = &cephclient.MonInfo{Name: "ext-mon-id", Endpoint: "0.0.0.0:6789"}
	monQuorumResponse = clienttest.MonInQuorumResponseFromMons(mons)

	// internal mons and deployments has not changed
	// and unknown mon was removed from quorum
	err = c.checkHealth(ctx)
	assert.Nil(t, err)
	assert.Equal(t, 5, len(c.ClusterInfo.InternalMonitors))
	assert.Empty(t, c.ClusterInfo.ExternalMons)

	// No updates in unit tests w/ workaround
	assert.ElementsMatch(t, []string{}, testopk8s.DeploymentNamesUpdated(deploymentsUpdated))
	testopk8s.ClearDeploymentsUpdated(deploymentsUpdated)

	// check that unknown mon is not in endpoint configmap
	cm, err := c.context.Clientset.CoreV1().ConfigMaps(c.Namespace).Get(ctx, EndpointConfigMapName, metav1.GetOptions{})
	assert.Nil(t, err)
	assert.Empty(t, cm.Data[EndpointExternalMonsKey])
	monsFromCM := opcontroller.ParseMonEndpoints(cm.Data[EndpointDataKey])
	assert.Len(t, monsFromCM, 5)
	for id, mon := range monsFromCM {
		assert.Equal(t, inital5Mons[id].Name, mon.Name)
		assert.Equal(t, inital5Mons[id].Endpoint, mon.Endpoint)
		assert.Equal(t, inital5Mons[id].OutOfQuorum, mon.OutOfQuorum)
	}

	// 3. downscale mons to 4:
	c.spec.Mon.Count = 4
	monQuorumResponse = clienttest.MonInQuorumResponseFromMons(c.ClusterInfo.InternalMonitors)

	err = c.checkHealth(ctx)
	assert.Nil(t, err)
	// todo fix
	assert.Equal(t, 4, len(c.ClusterInfo.InternalMonitors))
	assert.Empty(t, c.ClusterInfo.ExternalMons)
	// No updates in unit tests w/ workaround
	assert.ElementsMatch(t, []string{}, testopk8s.DeploymentNamesUpdated(deploymentsUpdated))
	testopk8s.ClearDeploymentsUpdated(deploymentsUpdated)

	// check that unknown mon is not in endpoint configmap
	cm, err = c.context.Clientset.CoreV1().ConfigMaps(c.Namespace).Get(ctx, EndpointConfigMapName, metav1.GetOptions{})
	assert.Nil(t, err)
	assert.Empty(t, cm.Data[EndpointExternalMonsKey])
	monsFromCM = opcontroller.ParseMonEndpoints(cm.Data[EndpointDataKey])
	assert.Len(t, monsFromCM, 4)
	for id, mon := range monsFromCM {
		assert.Equal(t, inital5Mons[id].Name, mon.Name)
		assert.Equal(t, inital5Mons[id].Endpoint, mon.Endpoint)
		assert.Equal(t, inital5Mons[id].OutOfQuorum, mon.OutOfQuorum)
	}

	// 4. upscale back to 5:
	c.spec.Mon.Count = 5
	monQuorumResponse = clienttest.MonInQuorumResponseFromMons(c.ClusterInfo.InternalMonitors)

	err = c.checkHealth(ctx)
	assert.Nil(t, err)
	assert.Equal(t, 5, len(c.ClusterInfo.InternalMonitors))
	assert.Empty(t, c.ClusterInfo.ExternalMons)
	// No updates in unit tests w/ workaround
	assert.Len(t, testopk8s.DeploymentNamesUpdated(deploymentsUpdated), 4)
	testopk8s.ClearDeploymentsUpdated(deploymentsUpdated)

	// check that unknown mon is not in endpoint configmap
	cm, err = c.context.Clientset.CoreV1().ConfigMaps(c.Namespace).Get(ctx, EndpointConfigMapName, metav1.GetOptions{})
	assert.Nil(t, err)
	assert.Empty(t, cm.Data[EndpointExternalMonsKey])
	monsFromCM = opcontroller.ParseMonEndpoints(cm.Data[EndpointDataKey])
	assert.Len(t, monsFromCM, 5)
	for id, mon := range monsFromCM {
		assert.Equal(t, c.ClusterInfo.InternalMonitors[id].Name, mon.Name)
		assert.Equal(t, c.ClusterInfo.InternalMonitors[id].Endpoint, mon.Endpoint)
		assert.Equal(t, c.ClusterInfo.InternalMonitors[id].OutOfQuorum, mon.OutOfQuorum)
	}
}

func TestExternalMons_inSpec_notInQuorum(t *testing.T) {
	// 1. setup test
	ctx := context.TODO()
	var deploymentsUpdated *[]*apps.Deployment
	updateDeploymentAndWait, deploymentsUpdated = testopk8s.UpdateDeploymentAndWaitStub()

	monQuorumResponse := clienttest.MonInQuorumResponse()
	executor := &exectest.MockExecutor{
		MockExecuteCommandWithOutput: func(command string, args ...string) (string, error) {
			logger.Infof("executing command: %s %+v", command, args)
			if args[0] == "auth" && args[1] == "get-or-create-key" {
				return "{\"key\":\"mysecurekey\"}", nil
			}
			return monQuorumResponse, nil
		},
	}
	clientset := test.New(t, 1)
	configDir := t.TempDir()
	context := &clusterd.Context{
		Clientset: clientset,
		ConfigDir: configDir,
		Executor:  executor,
	}
	ownerInfo := cephclient.NewMinimumOwnerInfoWithOwnerRef()
	c := New(ctx, context, "ns", cephv1.ClusterSpec{}, ownerInfo)
	setCommonMonProperties(c, 0, cephv1.MonSpec{Count: 5, AllowMultiplePerNode: true}, "myversion")
	c.maxMonID = 0 // "a" is max mon id
	c.waitForStart = false

	// checking the health will increase the mons as desired all in one go
	err := c.checkHealth(ctx)
	assert.Nil(t, err)
	assert.Equal(t, 5, len(c.ClusterInfo.InternalMonitors), fmt.Sprintf("mons: %v", c.ClusterInfo.InternalMonitors))
	assert.ElementsMatch(t, []string{
		// b is created first, no updates
		"rook-ceph-mon-b",                    // b updated when c created
		"rook-ceph-mon-b", "rook-ceph-mon-c", // b and c updated when d created
		"rook-ceph-mon-b", "rook-ceph-mon-c", "rook-ceph-mon-d", // etc.
		"rook-ceph-mon-b", "rook-ceph-mon-c", "rook-ceph-mon-d", "rook-ceph-mon-e",
	},
		testopk8s.DeploymentNamesUpdated(deploymentsUpdated))
	testopk8s.ClearDeploymentsUpdated(deploymentsUpdated)

	inital5Mons := make(map[string]*cephclient.MonInfo)
	for k, v := range c.ClusterInfo.InternalMonitors {
		inital5Mons[k] = v
	}

	// 2. add external mon id to spec but not to quorum
	c.spec.Mon.ExternalMonIDs = []string{"ext-mon-id"}

	// don't add ext mon to quorum:
	monQuorumResponse = clienttest.MonInQuorumResponseFromMons(inital5Mons)

	err = c.checkHealth(ctx)
	assert.Nil(t, err)
	assert.Equal(t, 5, len(c.ClusterInfo.InternalMonitors))
	assert.Empty(t, c.ClusterInfo.ExternalMons)

	// No updates in unit tests w/ workaround
	assert.ElementsMatch(t, []string{}, testopk8s.DeploymentNamesUpdated(deploymentsUpdated))
	testopk8s.ClearDeploymentsUpdated(deploymentsUpdated)

	// check that unknown mon is not in endpoint configmap
	cm, err := c.context.Clientset.CoreV1().ConfigMaps(c.Namespace).Get(ctx, EndpointConfigMapName, metav1.GetOptions{})
	assert.Nil(t, err)
	assert.Empty(t, cm.Data[EndpointExternalMonsKey])
	monsFromCM := opcontroller.ParseMonEndpoints(cm.Data[EndpointDataKey])
	assert.Len(t, monsFromCM, 5)
	for id, mon := range monsFromCM {
		assert.Equal(t, inital5Mons[id].Name, mon.Name)
		assert.Equal(t, inital5Mons[id].Endpoint, mon.Endpoint)
		assert.Equal(t, inital5Mons[id].OutOfQuorum, mon.OutOfQuorum)
	}

	// 3. downscale mons to 4:
	c.spec.Mon.Count = 4

	err = c.checkHealth(ctx)
	assert.Nil(t, err)
	assert.Equal(t, 4, len(c.ClusterInfo.InternalMonitors))
	assert.Empty(t, c.ClusterInfo.ExternalMons)
	// No updates in unit tests w/ workaround
	assert.ElementsMatch(t, []string{}, testopk8s.DeploymentNamesUpdated(deploymentsUpdated))
	testopk8s.ClearDeploymentsUpdated(deploymentsUpdated)

	// check that unknown mon is not in endpoint configmap
	cm, err = c.context.Clientset.CoreV1().ConfigMaps(c.Namespace).Get(ctx, EndpointConfigMapName, metav1.GetOptions{})
	assert.Nil(t, err)
	assert.Empty(t, cm.Data[EndpointExternalMonsKey])
	monsFromCM = opcontroller.ParseMonEndpoints(cm.Data[EndpointDataKey])
	assert.Len(t, monsFromCM, 4)
	for id, mon := range monsFromCM {
		assert.Equal(t, inital5Mons[id].Name, mon.Name)
		assert.Equal(t, inital5Mons[id].Endpoint, mon.Endpoint)
		assert.Equal(t, inital5Mons[id].OutOfQuorum, mon.OutOfQuorum)
	}

	// 4. upscale back to 5:
	c.spec.Mon.Count = 5
	monQuorumResponse = clienttest.MonInQuorumResponseFromMons(c.ClusterInfo.InternalMonitors)

	err = c.checkHealth(ctx)
	assert.Nil(t, err)
	assert.Equal(t, 5, len(c.ClusterInfo.InternalMonitors))
	assert.Empty(t, c.ClusterInfo.ExternalMons)
	// No updates in unit tests w/ workaround
	assert.Len(t, testopk8s.DeploymentNamesUpdated(deploymentsUpdated), 4)
	testopk8s.ClearDeploymentsUpdated(deploymentsUpdated)

	// check that unknown mon is not in endpoint configmap
	cm, err = c.context.Clientset.CoreV1().ConfigMaps(c.Namespace).Get(ctx, EndpointConfigMapName, metav1.GetOptions{})
	assert.Nil(t, err)
	assert.Empty(t, cm.Data[EndpointExternalMonsKey])
	monsFromCM = opcontroller.ParseMonEndpoints(cm.Data[EndpointDataKey])
	assert.Len(t, monsFromCM, 5)
	for id, mon := range monsFromCM {
		assert.Equal(t, c.ClusterInfo.InternalMonitors[id].Name, mon.Name)
		assert.Equal(t, c.ClusterInfo.InternalMonitors[id].Endpoint, mon.Endpoint)
		assert.Equal(t, c.ClusterInfo.InternalMonitors[id].OutOfQuorum, mon.OutOfQuorum)
	}
}

func TestExternalMons_inSpec_inQuorum(t *testing.T) {
	// 1. setup test
	ctx := context.TODO()
	var deploymentsUpdated *[]*apps.Deployment
	updateDeploymentAndWait, deploymentsUpdated = testopk8s.UpdateDeploymentAndWaitStub()

	monQuorumResponse := clienttest.MonInQuorumResponse()
	executor := &exectest.MockExecutor{
		MockExecuteCommandWithOutput: func(command string, args ...string) (string, error) {
			logger.Infof("executing command: %s %+v", command, args)
			if args[0] == "auth" && args[1] == "get-or-create-key" {
				return "{\"key\":\"mysecurekey\"}", nil
			}
			return monQuorumResponse, nil
		},
	}
	clientset := test.New(t, 1)
	configDir := t.TempDir()
	context := &clusterd.Context{
		Clientset: clientset,
		ConfigDir: configDir,
		Executor:  executor,
	}
	ownerInfo := cephclient.NewMinimumOwnerInfoWithOwnerRef()
	c := New(ctx, context, "ns", cephv1.ClusterSpec{}, ownerInfo)
	setCommonMonProperties(c, 0, cephv1.MonSpec{Count: 5, AllowMultiplePerNode: true}, "myversion")
	c.maxMonID = 0 // "a" is max mon id
	c.waitForStart = false

	// checking the health will increase the mons as desired all in one go
	err := c.checkHealth(ctx)
	assert.Nil(t, err)
	assert.Equal(t, 5, len(c.ClusterInfo.InternalMonitors), fmt.Sprintf("mons: %v", c.ClusterInfo.InternalMonitors))
	assert.ElementsMatch(t, []string{
		// b is created first, no updates
		"rook-ceph-mon-b",                    // b updated when c created
		"rook-ceph-mon-b", "rook-ceph-mon-c", // b and c updated when d created
		"rook-ceph-mon-b", "rook-ceph-mon-c", "rook-ceph-mon-d", // etc.
		"rook-ceph-mon-b", "rook-ceph-mon-c", "rook-ceph-mon-d", "rook-ceph-mon-e",
	},
		testopk8s.DeploymentNamesUpdated(deploymentsUpdated))
	testopk8s.ClearDeploymentsUpdated(deploymentsUpdated)

	inital5Mons := make(map[string]*cephclient.MonInfo)
	for k, v := range c.ClusterInfo.InternalMonitors {
		inital5Mons[k] = v
	}

	// 2. add external mon id to spec
	c.spec.Mon.ExternalMonIDs = []string{"ext-mon-id"}

	// add ext mon to quorum:
	mons := make(map[string]*cephclient.MonInfo)
	for k, v := range inital5Mons {
		mons[k] = v
	}
	mons["ext-mon-id"] = &cephclient.MonInfo{Name: "ext-mon-id", Endpoint: "0.0.0.0:6789"}
	monQuorumResponse = clienttest.MonInQuorumResponseFromMons(mons)

	// internal mons and deployments has not changed
	err = c.checkHealth(ctx)
	assert.Nil(t, err)
	assert.Equal(t, 5, len(c.ClusterInfo.InternalMonitors))
	// external mon is in quorum
	assert.Len(t, c.ClusterInfo.ExternalMons, 1)
	assert.Equal(t, "ext-mon-id", c.ClusterInfo.ExternalMons["ext-mon-id"].Name)

	// No updates in unit tests w/ workaround
	assert.ElementsMatch(t, []string{}, testopk8s.DeploymentNamesUpdated(deploymentsUpdated))
	testopk8s.ClearDeploymentsUpdated(deploymentsUpdated)

	// check that ext mon is in endpoint configmap
	cm, err := c.context.Clientset.CoreV1().ConfigMaps(c.Namespace).Get(ctx, EndpointConfigMapName, metav1.GetOptions{})
	assert.Nil(t, err)
	assert.Equal(t, "ext-mon-id", cm.Data[EndpointExternalMonsKey])
	monsFromCM := opcontroller.ParseMonEndpoints(cm.Data[EndpointDataKey])
	assert.Len(t, monsFromCM, 6)
	for id, mon := range monsFromCM {
		if id == "ext-mon-id" {
			assert.Equal(t, c.ClusterInfo.ExternalMons[id].Name, mon.Name)
			assert.Equal(t, c.ClusterInfo.ExternalMons[id].Endpoint, mon.Endpoint)
			assert.Equal(t, c.ClusterInfo.ExternalMons[id].OutOfQuorum, mon.OutOfQuorum)
		} else {
			assert.Equal(t, inital5Mons[id].Name, mon.Name)
			assert.Equal(t, inital5Mons[id].Endpoint, mon.Endpoint)
			assert.Equal(t, inital5Mons[id].OutOfQuorum, mon.OutOfQuorum)
		}
	}

	// 3. downscale mons to 4:
	c.spec.Mon.Count = 4

	err = c.checkHealth(ctx)
	assert.Nil(t, err)
	assert.Equal(t, 4, len(c.ClusterInfo.InternalMonitors))
	assert.Len(t, c.ClusterInfo.ExternalMons, 1)
	// No updates in unit tests w/ workaround
	assert.ElementsMatch(t, []string{}, testopk8s.DeploymentNamesUpdated(deploymentsUpdated))
	testopk8s.ClearDeploymentsUpdated(deploymentsUpdated)

	// check that unknown mon is not in endpoint configmap
	cm, err = c.context.Clientset.CoreV1().ConfigMaps(c.Namespace).Get(ctx, EndpointConfigMapName, metav1.GetOptions{})
	assert.Nil(t, err)
	assert.Equal(t, "ext-mon-id", cm.Data[EndpointExternalMonsKey])
	monsFromCM = opcontroller.ParseMonEndpoints(cm.Data[EndpointDataKey])
	assert.Len(t, monsFromCM, 5)
	for id, mon := range monsFromCM {
		if id == "ext-mon-id" {
			assert.Equal(t, c.ClusterInfo.ExternalMons[id].Name, mon.Name)
			assert.Equal(t, c.ClusterInfo.ExternalMons[id].Endpoint, mon.Endpoint)
			assert.Equal(t, c.ClusterInfo.ExternalMons[id].OutOfQuorum, mon.OutOfQuorum)
		} else {
			assert.Equal(t, inital5Mons[id].Name, mon.Name)
			assert.Equal(t, inital5Mons[id].Endpoint, mon.Endpoint)
			assert.Equal(t, inital5Mons[id].OutOfQuorum, mon.OutOfQuorum)
		}
	}

	// 4. upscale back to 5:
	c.spec.Mon.Count = 5
	mons = make(map[string]*cephclient.MonInfo)
	for k, v := range c.ClusterInfo.InternalMonitors {
		mons[k] = v
	}
	mons["ext-mon-id"] = &cephclient.MonInfo{Name: "ext-mon-id", Endpoint: "0.0.0.0:6789"}
	monQuorumResponse = clienttest.MonInQuorumResponseFromMons(mons)

	err = c.checkHealth(ctx)
	assert.Nil(t, err)
	assert.Equal(t, 5, len(c.ClusterInfo.InternalMonitors))
	assert.Len(t, c.ClusterInfo.ExternalMons, 1)
	// No updates in unit tests w/ workaround
	assert.Len(t, testopk8s.DeploymentNamesUpdated(deploymentsUpdated), 4)
	testopk8s.ClearDeploymentsUpdated(deploymentsUpdated)

	// check that unknown mon is not in endpoint configmap
	cm, err = c.context.Clientset.CoreV1().ConfigMaps(c.Namespace).Get(ctx, EndpointConfigMapName, metav1.GetOptions{})
	assert.Nil(t, err)
	assert.Equal(t, "ext-mon-id", cm.Data[EndpointExternalMonsKey])
	monsFromCM = opcontroller.ParseMonEndpoints(cm.Data[EndpointDataKey])
	assert.Len(t, monsFromCM, 6)
	for id, mon := range monsFromCM {
		if id == "ext-mon-id" {
			assert.Equal(t, c.ClusterInfo.ExternalMons[id].Name, mon.Name)
			assert.Equal(t, c.ClusterInfo.ExternalMons[id].Endpoint, mon.Endpoint)
			assert.Equal(t, c.ClusterInfo.ExternalMons[id].OutOfQuorum, mon.OutOfQuorum)
		} else {
			assert.Equal(t, c.ClusterInfo.InternalMonitors[id].Name, mon.Name)
			assert.Equal(t, c.ClusterInfo.InternalMonitors[id].Endpoint, mon.Endpoint)
			assert.Equal(t, c.ClusterInfo.InternalMonitors[id].OutOfQuorum, mon.OutOfQuorum)
		}
	}
}
