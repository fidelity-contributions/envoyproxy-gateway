// Copyright Envoy Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	egv1a1 "github.com/envoyproxy/gateway/api/v1alpha1"
	"github.com/envoyproxy/gateway/internal/envoygateway"
	"github.com/envoyproxy/gateway/internal/envoygateway/config"
	"github.com/envoyproxy/gateway/internal/gatewayapi"
	"github.com/envoyproxy/gateway/internal/gatewayapi/resource"
	"github.com/envoyproxy/gateway/internal/infrastructure/kubernetes/proxy"
	"github.com/envoyproxy/gateway/internal/ir"
)

const (
	testGatewayClass = "envoy-gateway-class"
	testResourceUID  = "foo.bar"
)

func newTestInfra(t *testing.T) *Infra {
	cli := fakeclient.NewClientBuilder().
		WithScheme(envoygateway.GetScheme()).
		WithInterceptorFuncs(interceptorFunc).
		Build()
	return newTestInfraWithClient(t, cli)
}

// Borrowing the interceptor from https://github.com/istio/istio/blob/2f54c6a52a5c6661d5eb9bd2277aab77304fee45/operator/pkg/helmreconciler/apply_test.go#L40
// Interceptor is used for ApplyPatch as of this patch is not yet supported by the fake client, see https://github.com/kubernetes/kubernetes/issues/99953
var interceptorFunc = interceptor.Funcs{Patch: func(
	ctx context.Context,
	clnt client.WithWatch,
	obj client.Object,
	patch client.Patch,
	opts ...client.PatchOption,
) error {
	// Apply patches are supposed to upsert, but fake client fails if the object doesn't exist,
	// if an apply patch occurs for an object that doesn't yet exist, create it.
	if patch.Type() != types.ApplyPatchType {
		return clnt.Patch(ctx, obj, patch, opts...)
	}
	check, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return errors.New("could not check for object in fake client")
	}
	if err := clnt.Get(ctx, client.ObjectKeyFromObject(obj), check); kerrors.IsNotFound(err) {
		if err := clnt.Create(ctx, check); err != nil {
			return fmt.Errorf("could not inject object creation for fake: %w", err)
		}
	} else if err != nil {
		return err
	}
	obj.SetResourceVersion(check.GetResourceVersion())
	return clnt.Update(ctx, obj)
}}

func TestCmpBytes(t *testing.T) {
	m1 := map[string][]byte{}
	m1["a"] = []byte("aaa")
	m2 := map[string][]byte{}
	m2["a"] = []byte("aaa")

	assert.True(t, reflect.DeepEqual(m1, m2))
	assert.False(t, reflect.DeepEqual(nil, m2))
	assert.False(t, reflect.DeepEqual(m1, nil))
}

func newTestInfraWithClient(t *testing.T, cli client.Client) *Infra {
	cfg, err := config.New(os.Stdout)
	require.NoError(t, err)

	cfg.EnvoyGateway = &egv1a1.EnvoyGateway{
		TypeMeta: metav1.TypeMeta{},
		EnvoyGatewaySpec: egv1a1.EnvoyGatewaySpec{
			RateLimit: &egv1a1.RateLimit{
				Backend: egv1a1.RateLimitDatabaseBackend{
					Type: egv1a1.RedisBackendType,
					Redis: &egv1a1.RateLimitRedisSettings{
						URL: "",
						TLS: &egv1a1.RedisTLSSettings{
							CertificateRef: &gwapiv1.SecretObjectReference{
								Name: "ratelimit-cert",
							},
						},
					},
				},
			},
		},
	}

	return NewInfra(cli, cfg)
}

func TestCreateProxyInfra(t *testing.T) {
	infra := ir.NewInfra()
	infra.GetProxyInfra().GetProxyMetadata().OwnerReference = &ir.ResourceMetadata{
		Kind: resource.KindGateway,
		Name: testGatewayClass,
	}

	// Infra with Gateway owner labels.
	infraWithLabels := infra.DeepCopy()
	infraWithLabels.GetProxyInfra().GetProxyMetadata().Labels = proxy.EnvoyAppLabel()
	infraWithLabels.GetProxyInfra().GetProxyMetadata().Labels[gatewayapi.OwningGatewayClassLabel] = "testGatewayClass"
	infraWithLabels.GetProxyInfra().GetProxyMetadata().Labels[gatewayapi.OwningGatewayNamespaceLabel] = "default"
	infraWithLabels.GetProxyInfra().GetProxyMetadata().Labels[gatewayapi.OwningGatewayNameLabel] = "test-gw"
	infraWithLabels.GetProxyInfra().GetProxyMetadata().OwnerReference = &ir.ResourceMetadata{
		Kind: resource.KindGateway,
		Name: testGatewayClass,
	}

	ep := &egv1a1.EnvoyProxy{
		Spec: egv1a1.EnvoyProxySpec{
			Provider: &egv1a1.EnvoyProxyProvider{
				Type:       egv1a1.ProviderTypeKubernetes,
				Kubernetes: egv1a1.DefaultEnvoyProxyKubeProvider(),
			},
		},
	}
	infraWithPDB := infraWithLabels.DeepCopy()
	infraWithPDB.GetProxyInfra().Config = ep.DeepCopy()
	infraWithPDB.GetProxyInfra().Config.Spec.Provider.Kubernetes.EnvoyPDB = &egv1a1.KubernetesPodDisruptionBudgetSpec{
		MinAvailable: ptr.To(intstr.IntOrString{Type: intstr.Int, IntVal: 1}),
	}

	infraWithHPA := infraWithLabels.DeepCopy()
	infraWithHPA.GetProxyInfra().Config = ep.DeepCopy()
	infraWithHPA.GetProxyInfra().Config.Spec.Provider.Kubernetes.EnvoyHpa = &egv1a1.KubernetesHorizontalPodAutoscalerSpec{
		MinReplicas: ptr.To[int32](1),
	}

	testCases := []struct {
		name   string
		in     *ir.Infra
		expect bool
	}{
		{
			name:   "infra-with-expected-labels",
			in:     infraWithLabels,
			expect: true,
		},
		{
			name:   "default infra without Gateway owner labels",
			in:     infra,
			expect: false,
		},
		{
			name:   "nil-infra",
			in:     nil,
			expect: false,
		},
		{
			name: "nil-infra-proxy",
			in: &ir.Infra{
				Proxy: nil,
			},
			expect: false,
		},
		{
			name:   "pdb enabled",
			in:     infraWithPDB,
			expect: true,
		},
		{
			name:   "hpa enabled",
			in:     infraWithHPA,
			expect: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			kube := newTestInfra(t)
			require.NoError(t, setupOwnerReferenceResources(context.Background(), kube.Client))
			// Create or update the proxy infra.
			err := kube.CreateOrUpdateProxyInfra(context.Background(), tc.in)
			if !tc.expect {
				require.Error(t, err)
			} else {
				require.NoError(t, err)

				// Verify all resources were created via the fake kube client.
				sa := &corev1.ServiceAccount{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: kube.ControllerNamespace,
						Name:      expectedName(tc.in.Proxy, false),
					},
				}
				require.NoError(t, kube.Client.Get(context.Background(), client.ObjectKeyFromObject(sa), sa))

				cm := &corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: kube.ControllerNamespace,
						Name:      expectedName(tc.in.Proxy, false),
					},
				}
				require.NoError(t, kube.Client.Get(context.Background(), client.ObjectKeyFromObject(cm), cm))

				deploy := &appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: kube.ControllerNamespace,
						Name:      expectedName(tc.in.Proxy, false),
					},
				}
				require.NoError(t, kube.Client.Get(context.Background(), client.ObjectKeyFromObject(deploy), deploy))

				svc := &corev1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: kube.ControllerNamespace,
						Name:      expectedName(tc.in.Proxy, false),
					},
				}
				require.NoError(t, kube.Client.Get(context.Background(), client.ObjectKeyFromObject(svc), svc))
			}
		})
	}
}

func TestDeleteProxyInfra(t *testing.T) {
	infra := ir.NewInfra()
	infra.GetProxyInfra().GetProxyMetadata().OwnerReference = &ir.ResourceMetadata{
		Kind: resource.KindGatewayClass,
		Name: testGatewayClass,
	}

	testCases := []struct {
		name   string
		in     *ir.Infra
		expect bool
	}{
		{
			name:   "nil infra",
			in:     nil,
			expect: false,
		},
		{
			name:   "default infra",
			in:     infra,
			expect: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			kube := newTestInfra(t)
			require.NoError(t, setupOwnerReferenceResources(context.Background(), kube.Client))

			err := kube.DeleteProxyInfra(context.Background(), tc.in)
			if !tc.expect {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// This function uses setup creating Resources for OwnerReference.
// When the default case, ProxyInfra Get OwnerReference from GatewayClass.
// When enable GatewayNamespace mode, ProxyInfra Get OwnerReference from Gateway.
func setupOwnerReferenceResources(ctx context.Context, client *InfraClient) error {
	gwc := &gwapiv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: testGatewayClass,
			UID:  testResourceUID,
		},
	}
	if err := client.Create(ctx, gwc); err != nil {
		return err
	}
	gw := &gwapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns1",
			Name:      "gateway-1",
			UID:       testResourceUID,
		},
	}
	return client.Create(ctx, gw)
}
