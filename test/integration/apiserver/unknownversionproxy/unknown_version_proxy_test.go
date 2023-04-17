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
	"fmt"
	"testing"
	//"time"

	//"time"

	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/features"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/kubernetes"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	kastesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"
	kubeapiservertesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"
	"k8s.io/kubernetes/test/integration/framework"
	"k8s.io/kubernetes/test/utils/ktesting"

	//"k8s.io/kubernetes/test/utils/ktesting"
)

func TestUnknownVersionProxiedRequest(t *testing.T) {

	ktesting.SetDefaultVerbosity(1)
	// enable feature flags
	defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.APIServerIdentity, true)()
	defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.StorageVersionAPI, true)()

	// create sharedetcd
	etcd := framework.SharedEtcd()
	// start test server with all APIs enabled
	serverA := kubeapiservertesting.StartTestServerOrDie(t, &kastesting.TestServerInstanceOptions{EnableCertAuth: false}, []string{fmt.Sprintf("--v=%v", "22")}, etcd)
	//serverA := kubeapiservertesting.StartTestServerOrDie(t, nil, nil, etcd)
	defer serverA.TearDownFn()

	// start another test server with some api disabled
	serverB := kubeapiservertesting.StartTestServerOrDie(t, &kastesting.TestServerInstanceOptions{EnableCertAuth: false}, []string{ fmt.Sprintf("--v=%v", "22"),
		fmt.Sprintf("--runtime-config=%v", "batch/v1=false")}, etcd)
	defer serverB.TearDownFn()

	kubeClientSetA, err := kubernetes.NewForConfig(serverA.ClientConfig)
	fmt.Printf("RICHAA serverA client config: %v",serverA.ClientConfig)
	require.NoError(t, err)

	kubeClientSetB, err := kubernetes.NewForConfig(serverB.ClientConfig)
	require.NoError(t, err)

	cr := &v1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-admin",
		},
		Rules: []v1.PolicyRule{
			{
				Verbs:         []string{"*"},
				APIGroups:     []string{""},
				Resources:     []string{"*"},
			},
		},
	}

	crb := &v1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: cr.Name,
			Namespace: "kube-system",
		},
		Subjects: []v1.Subject{
			{
				Kind: v1.UserKind,
				Name: "system:masters",
				APIGroup: "rbac.authorization.k8s.io",
			},
		},
		RoleRef: v1.RoleRef{
			Kind:     "ClusterRole",
			APIGroup: "",
			Name:     "cluster-admin",
		},
	}
	/*if _, err := kubeClientSetA.RbacV1().ClusterRoles().Create(context.TODO(), cr, metav1.CreateOptions{}); err != nil {
		t.Fatalf("unable to create cluster role binding: %v", err)
	}*/
	if _, err := kubeClientSetA.RbacV1().ClusterRoleBindings().Create(context.TODO(), crb, metav1.CreateOptions{}); err != nil {
		t.Fatalf("unable to create cluster role binding: %v", err)
	}

	fmt.Printf("RICHAA created clusterrolebinding successfully")

	// create jobs resource
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-job",
			Namespace: "kube-system",
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "test",
							Image: "test",
						},
					},
					RestartPolicy: corev1.RestartPolicyNever,
				},
			},
		},
	}
	_, err = kubeClientSetA.BatchV1().Jobs("kube-system").Create(context.Background(), job, metav1.CreateOptions{})
	if err != nil {
		t.Errorf("ServerA: failed to create jobs  %v",err)
	} else {
		fmt.Printf("ServerA has created jobs")
	}

	// list jobs using ServerA
	/*jobsA, err := kubeClientSetA.BatchV1().Jobs("kube-system").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Errorf("ServerA: failed to list jobs  %v",err)
	}
	fmt.Printf("JobsA list length retrieved from ServerA %v", len(jobsA.Items))*/

	//time.Sleep(10 * time.Minute)

	// list jobs using ServerB
	jobsB, err := kubeClientSetB.BatchV1().Jobs("kube-system").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Errorf("ServerB: failed to list jobs  %v",err)
	}
	fmt.Printf("JobsB list length %v", len(jobsB.Items))

}
