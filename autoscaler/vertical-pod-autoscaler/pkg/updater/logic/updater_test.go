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

package logic

import (
	"strconv"
	"testing"

	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	vpa_types "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1beta1"
	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/updater/eviction"
	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/utils/test"
	"k8s.io/kubernetes/pkg/api/testapi"
)

func TestRunOnce(t *testing.T) {
	replicas := int32(5)
	livePods := 5
	labels := map[string]string{"app": "testingApp"}
	selector := "app = testingApp"
	containerName := "container1"
	rc := apiv1.ReplicationController{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "rc",
			Namespace: "default",
			SelfLink:  testapi.Default.SelfLink("replicationcontrollers", "rc"),
		},
		Spec: apiv1.ReplicationControllerSpec{
			Replicas: &replicas,
		},
	}
	pods := make([]*apiv1.Pod, livePods)
	eviction := &test.PodsEvictionRestrictionMock{}

	for i := range pods {
		pods[i] = test.Pod().WithName("test_"+strconv.Itoa(i)).AddContainer(test.BuildTestContainer(containerName, "1", "100M")).WithCreator(&rc.ObjectMeta, &rc.TypeMeta).Get()

		pods[i].Labels = labels
		eviction.On("CanEvict", pods[i]).Return(true)
		eviction.On("Evict", pods[i], nil).Return(nil)
	}

	factory := &fakeEvictFactory{eviction}
	vpaLister := &test.VerticalPodAutoscalerListerMock{}

	podLister := &test.PodListerMock{}
	podLister.On("List").Return(pods, nil)

	vpaObj := test.VerticalPodAutoscaler().
		WithContainer(containerName).
		WithTarget("2", "200M").
		WithMinAllowed("1", "100M").
		WithMaxAllowed("3", "1G").
		WithSelector(selector).Get()
	updateMode := vpa_types.UpdateModeAuto
	vpaObj.Spec.UpdatePolicy = &vpa_types.PodUpdatePolicy{UpdateMode: &updateMode}
	vpaLister.On("List").Return([]*vpa_types.VerticalPodAutoscaler{vpaObj}, nil).Once()

	updater := &updater{
		vpaLister:               vpaLister,
		podLister:               podLister,
		evictionFactory:         factory,
		recommendationProcessor: &test.FakeRecommendationProcessor{},
	}

	updater.RunOnce()
	eviction.AssertNumberOfCalls(t, "Evict", 5)
}

func TestVPAOff(t *testing.T) {
	livePods := 5
	labels := map[string]string{"app": "testingApp"}
	selector := "app = testingApp"
	containerName := "container1"
	pods := make([]*apiv1.Pod, livePods)
	eviction := &test.PodsEvictionRestrictionMock{}

	for i := range pods {
		pods[i] = test.Pod().WithName("test_" + strconv.Itoa(i)).AddContainer(test.BuildTestContainer(containerName, "1", "100M")).Get()
		pods[i].Labels = labels
		eviction.On("CanEvict", pods[i]).Return(true)
		eviction.On("Evict", pods[i], nil).Return(nil)
	}

	factory := &fakeEvictFactory{eviction}
	vpaLister := &test.VerticalPodAutoscalerListerMock{}

	podLister := &test.PodListerMock{}
	podLister.On("List").Return(pods, nil)

	vpaObj := test.VerticalPodAutoscaler().
		WithContainer(containerName).
		WithTarget("2", "200M").
		WithMinAllowed("1", "100M").
		WithMaxAllowed("3", "1G").
		WithSelector(selector).Get()
	vpaObj.Namespace = "default"
	updateMode := vpa_types.UpdateModeInitial
	vpaObj.Spec.UpdatePolicy = &vpa_types.PodUpdatePolicy{UpdateMode: &updateMode}
	vpaLister.On("List").Return([]*vpa_types.VerticalPodAutoscaler{vpaObj}, nil).Once()

	updater := &updater{
		vpaLister:               vpaLister,
		podLister:               podLister,
		evictionFactory:         factory,
		recommendationProcessor: &test.FakeRecommendationProcessor{},
	}

	updater.RunOnce()
	eviction.AssertNumberOfCalls(t, "Evict", 0)
}

func TestRunOnceNotingToProcess(t *testing.T) {
	eviction := &test.PodsEvictionRestrictionMock{}
	factory := &fakeEvictFactory{eviction}
	vpaLister := &test.VerticalPodAutoscalerListerMock{}
	podLister := &test.PodListerMock{}
	vpaLister.On("List").Return(nil, nil).Once()

	updater := &updater{
		vpaLister:               vpaLister,
		podLister:               podLister,
		evictionFactory:         factory,
		recommendationProcessor: &test.FakeRecommendationProcessor{},
	}
	updater.RunOnce()
}

type fakeEvictFactory struct {
	evict eviction.PodsEvictionRestriction
}

func (f fakeEvictFactory) NewPodsEvictionRestriction(pods []*apiv1.Pod) eviction.PodsEvictionRestriction {
	return f.evict
}
