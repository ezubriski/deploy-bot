package reposcanner

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// K8sConfigMapWriter writes data to a Kubernetes ConfigMap using in-cluster
// credentials.
type K8sConfigMapWriter struct {
	client kubernetes.Interface
}

// NewK8sConfigMapWriter creates a ConfigMapWriter using in-cluster config.
func NewK8sConfigMapWriter() (*K8sConfigMapWriter, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("create clientset: %w", err)
	}
	return &K8sConfigMapWriter{client: clientset}, nil
}

// Write updates a ConfigMap data key. Returns true if the content changed.
// If namespace is empty, it reads the pod's namespace from the downward API.
func (w *K8sConfigMapWriter) Write(ctx context.Context, namespace, name, key string, data []byte) (bool, error) {
	if namespace == "" {
		namespace = inferNamespace()
	}

	existing, err := w.client.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		// ConfigMap doesn't exist yet — create it.
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
			Data: map[string]string{key: string(data)},
		}
		_, err = w.client.CoreV1().ConfigMaps(namespace).Create(ctx, cm, metav1.CreateOptions{})
		if err != nil {
			return false, fmt.Errorf("create configmap: %w", err)
		}
		return true, nil
	}

	// Compare SHA-256 to avoid no-op writes.
	oldHash := sha256.Sum256([]byte(existing.Data[key]))
	newHash := sha256.Sum256(data)
	if oldHash == newHash {
		return false, nil
	}

	if existing.Data == nil {
		existing.Data = make(map[string]string)
	}
	existing.Data[key] = string(data)
	_, err = w.client.CoreV1().ConfigMaps(namespace).Update(ctx, existing, metav1.UpdateOptions{})
	if err != nil {
		return false, fmt.Errorf("update configmap: %w", err)
	}
	return true, nil
}

// inferNamespace reads the namespace from the Kubernetes downward API file,
// falling back to "default".
func inferNamespace() string {
	data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		return "default"
	}
	return string(data)
}
