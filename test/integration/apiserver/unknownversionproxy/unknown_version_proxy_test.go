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

package unknownversionproxy

import (
	"context"
	"flag"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/features"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/kubernetes"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	kubeapiservertesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"
	"k8s.io/kubernetes/pkg/controlplane"
	"k8s.io/kubernetes/test/integration/framework"
	"syscall"
)

func TestUnknownVersionProxiedRequest(t *testing.T) {

	// enable feature flags
	defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.APIServerIdentity, true)()
	defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.StorageVersionAPI, true)()

	// create sharedetcd
	etcd := framework.SharedEtcd()
	// start test server with all APIs enabled
	serverA := kubeapiservertesting.StartTestServerOrDie(t, nil, nil, etcd)
	fmt.Printf("etcdA : %v", serverA.EtcdStoragePrefix)
	defer serverA.TearDownFn()

	// start another test server with some api disabled
	serverB := kubeapiservertesting.StartTestServerOrDie(t, nil, []string{
		fmt.Sprintf("--runtime-config=%v", "batch/v1=false")}, etcd)
	fmt.Printf("etcdB : %v", serverB.EtcdStoragePrefix)
	defer serverB.TearDownFn()

	//kubeClientSetA, err := kubernetes.NewForConfig(serverA.ClientConfig)
	//require.NoError(t, err)

	kubeClientSetB, err := kubernetes.NewForConfig(serverB.ClientConfig)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	/*req := kubeClientSetA.CoreV1().RESTClient().Get().Param("verbose","true").AbsPath("/readyz")
	req.SetHeader("Accept", "application/json")
	result, err := req.DoRaw(ctx)

	if err != nil {
		t.Fatalf("failed to get response: %v:\n%v", err, string(result))
	}

	fmt.Printf("Response %v", string(result))*/

	/*jobsA, err := kubeClientSetA.BatchV1().Jobs("kube-system").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Errorf("ServerA: failed to get response  %v",err)
	}
	fmt.Printf("JobsA list length %v", len(jobsA.Items))*/

	leases, err := kubeClientSetB.CoordinationV1().Leases("kube-system").List(ctx, metav1.ListOptions{LabelSelector: controlplane.KubeAPIServerIdentityLeaseLabelSelector})
	if err != nil {
		t.Errorf("ServerB: failed to get response  %v",err)
	}
	fmt.Printf("LeasesB list %v", leases)

	/*jobsB, err := kubeClientSetB.BatchV1().Jobs("kube-system").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Errorf("ServerB: failed to get response  %v",err)
	}
	fmt.Printf("JobsB list length %v", len(jobsB.Items))*/

}

func setHostname() {
	hostnamePtr := flag.String("hostname", "foo", "a string")
	flag.Parse()
	fmt.Println("hostame:", *hostnamePtr)
	err := syscall.Sethostname([]byte(*hostnamePtr))
	if err != nil {
		fmt.Println(err)
	} else {
		hostname, err := os.Hostname()
		if err != nil {
			fmt.Println(hostname)
		}
	}

}