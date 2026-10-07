package controller

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/onsi/gomega"
	"gopkg.in/yaml.v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	networkingv1alpha1 "github.com/adyanth/cloudflare-operator/api/v1alpha1"
	"github.com/adyanth/cloudflare-operator/internal/clients/cf"
)

func TestGetConfigForSubject(t *testing.T) {
	for _, tc := range []struct {
		name      string
		spec      networkingv1alpha1.TunnelBindingSubjectSpec
		service   *corev1.Service
		hostname  string
		target    string
		wantError bool
	}{
		{
			name:     "explicit target without a Service",
			spec:     networkingv1alpha1.TunnelBindingSubjectSpec{Fqdn: "*.example.com", Target: "http://gateway.other.svc:8080"},
			hostname: "*.example.com",
			target:   "http://gateway.other.svc:8080",
		},
		{
			name:     "explicit target with a generated hostname",
			spec:     networkingv1alpha1.TunnelBindingSubjectSpec{Target: "http_status:404"},
			hostname: "app.example.com",
			target:   "http_status:404",
		},
		{
			name: "explicit target ignores Service ports and protocol",
			spec: networkingv1alpha1.TunnelBindingSubjectSpec{
				Fqdn: "app.example.com", Target: "http://gateway.other.svc:8080", Protocol: "https",
			},
			service:  &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"}},
			hostname: "app.example.com",
			target:   "http://gateway.other.svc:8080",
		},
		{
			name: "Service target without an override",
			service: &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
				Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 443, Protocol: corev1.ProtocolTCP}}},
			},
			hostname: "app.example.com",
			target:   "https://app.default.svc:443",
		},
		{
			name:      "missing Service without an override",
			hostname:  "app.example.com",
			target:    "http_status:404",
			wantError: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			scheme := runtime.NewScheme()
			g.Expect(corev1.AddToScheme(scheme)).To(gomega.Succeed())
			builder := fake.NewClientBuilder().WithScheme(scheme)
			if tc.service != nil {
				builder.WithObjects(tc.service)
			}
			reconciler := TunnelBindingReconciler{
				Client:   builder.Build(),
				Recorder: record.NewFakeRecorder(10),
				ctx:      context.Background(),
				log:      logr.Discard(),
				binding:  &networkingv1alpha1.TunnelBinding{ObjectMeta: metav1.ObjectMeta{Namespace: "default"}},
				cfAPI:    &cf.API{Domain: "example.com"},
			}
			hostname, target, err := reconciler.getConfigForSubject(networkingv1alpha1.TunnelBindingSubject{Name: "app", Spec: tc.spec})
			g.Expect(err != nil).To(gomega.Equal(tc.wantError), "error = %v", err)
			g.Expect(hostname).To(gomega.Equal(tc.hostname))
			g.Expect(target).To(gomega.Equal(tc.target))
		})
	}
}

func TestTunnelBindingExplicitTargetStatusMatchesConfiguration(t *testing.T) {
	g := gomega.NewWithT(t)
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, networkingv1alpha1.AddToScheme} {
		g.Expect(add(scheme)).To(gomega.Succeed())
	}
	binding := &networkingv1alpha1.TunnelBinding{
		TypeMeta: metav1.TypeMeta{APIVersion: networkingv1alpha1.GroupVersion.String(), Kind: "TunnelBinding"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gateway",
			Namespace: "default",
			Labels:    map[string]string{tunnelNameLabel: "tunnel", tunnelKindLabel: "TunnelBinding"},
		},
		TunnelRef: networkingv1alpha1.TunnelRef{Name: "tunnel", Kind: "Tunnel"},
		Subjects: []networkingv1alpha1.TunnelBindingSubject{{
			Name: "wildcard",
			Spec: networkingv1alpha1.TunnelBindingSubjectSpec{Fqdn: "*.example.com", Target: "http://gateway.other.svc:8080"},
		}},
	}
	config := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "tunnel", Namespace: "default"},
		Data:       map[string]string{configmapKey: "tunnel: test\ningress: []\n"},
	}
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "tunnel", Namespace: "default"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(binding).WithObjects(binding, config, deployment).Build()
	recorder := record.NewFakeRecorder(10)
	reconciler := TunnelBindingReconciler{
		Client:         c,
		Recorder:       recorder,
		ctx:            context.Background(),
		log:            logr.Discard(),
		binding:        binding,
		configmap:      config,
		fallbackTarget: "http_status:404",
	}
	g.Expect(reconciler.setStatus()).To(gomega.Succeed())
	select {
	case event := <-recorder.Events:
		t.Fatalf("unexpected event while resolving explicit target: %s", event)
	default:
	}
	g.Expect(reconciler.configureCloudflareDaemon()).To(gomega.Succeed())
	stored := &networkingv1alpha1.TunnelBinding{}
	g.Expect(c.Get(context.Background(), client.ObjectKeyFromObject(binding), stored)).To(gomega.Succeed())
	g.Expect(stored.Status.Services).To(gomega.HaveLen(1))
	g.Expect(stored.Status.Services[0].Target).To(gomega.Equal(binding.Subjects[0].Spec.Target))
	var generated cf.Configuration
	g.Expect(yaml.Unmarshal([]byte(reconciler.configmap.Data[configmapKey]), &generated)).To(gomega.Succeed())
	g.Expect(generated.Ingress).To(gomega.HaveLen(2))
	g.Expect(generated.Ingress[0].Service).To(gomega.Equal(stored.Status.Services[0].Target))
	g.Expect(generated.Ingress[0].Hostname).To(gomega.Equal(stored.Status.Services[0].Hostname))
}
