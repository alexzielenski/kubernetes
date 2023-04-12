/*
Copyright 2015 The Kubernetes Authors.

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

package unknownversionproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apiserver/pkg/features"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/kubernetes"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	kubeapiservertesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"
	"k8s.io/kubernetes/test/integration/framework"
)

func TestUnknownVersionProxiedRequest(t *testing.T) {

	// enable feature flags
	defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.APIServerIdentity, true)()
	defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.StorageVersionAPI, true)()

	serverA := kubeapiservertesting.StartTestServerOrDie(t, nil, []string{
		fmt.Sprintf("--runtime-config=%v", "batch/v1=false")}, framework.SharedEtcd())
	defer serverA.TearDownFn()

	// start another server, with all APIs enabled
	serverB := kubeapiservertesting.StartTestServerOrDie(t, nil, nil, framework.SharedEtcd())
	defer serverB.TearDownFn()

	kubeClientSetA, err := kubernetes.NewForConfig(serverA.ClientConfig)
	require.NoError(t, err)

	request := kubeClientSetA.RESTClient().Get().AbsPath("/apis/batch/v1/jobs")
	request.SetHeader("Accept", "application/json")
	bytes, err := request.DoRaw(context.TODO())
	obj := &unstructured.Unstructured{}
	if err == nil {
		t.Errorf("expect server to reject request, but forwarded")
	}
	if err := json.Unmarshal(bytes, obj); err != nil {
		t.Errorf("error decoding %s: %v", "/apis/batch/v1/jobs", err)
	}

}
