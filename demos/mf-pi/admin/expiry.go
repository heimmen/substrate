// Per-user activation-expiry plumbing for the mf-pi admin server.
//
// Each mf-pi user is one Actor in the mfpi atespace. The admin can set an
// activation duration (e.g. "168h"); the effective absolute expiry timestamp
// (RFC3339) is stored here, and the background reconciler suspends the actor
// once its expiry passes so the worker/Pod resources are released (keeping the
// snapshot and user data, recoverable on next resume). Mirrors the persistence
// style of configMapPasswordStore: pre-created ConfigMap + namespace-scoped
// RBAC with only get/update, never create.
package main

import (
	"context"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// expiryStore persists per-user activation expiries. Implemented by
// configMapExpiryStore in production and by a fake in tests.
type expiryStore interface {
	// Get returns the stored expiry timestamp (RFC3339) for the user, and
	// whether one exists.
	Get(name string) (expiresAt string, ok bool)
	// Set stores (overwrites) the expiry timestamp for the user.
	Set(name, expiresAt string) error
	// Delete removes any stored expiry for the user (idempotent).
	Delete(name string) error
}

// configMapExpiryStore stores per-user expiry timestamps in a Kubernetes
// ConfigMap (username -> RFC3339 timestamp), mirroring them into an in-memory
// map. It mirrors configMapPasswordStore.
type configMapExpiryStore struct {
	clientset kubernetes.Interface
	namespace string
	name      string

	mu   sync.RWMutex
	data map[string]string // username -> expiresAt (RFC3339)
}

func newConfigMapExpiryStore(clientset kubernetes.Interface, namespace, name string) *configMapExpiryStore {
	return &configMapExpiryStore{
		clientset: clientset,
		namespace: namespace,
		name:      name,
		data:      map[string]string{},
	}
}

// load populates the in-memory map from the ConfigMap. A missing ConfigMap is
// treated as an empty store (not an error).
func (s *configMapExpiryStore) load(ctx context.Context) error {
	cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(ctx, s.name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = make(map[string]string, len(cm.Data))
	for k, v := range cm.Data {
		s.data[k] = v
	}
	return nil
}

func (s *configMapExpiryStore) Get(name string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.data[name]
	return v, ok
}

func (s *configMapExpiryStore) Set(name, expiresAt string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make(map[string]string, len(s.data)+1)
	for k, v := range s.data {
		next[k] = v
	}
	next[name] = expiresAt
	if err := s.persist(next); err != nil {
		return err
	}
	s.data = next
	return nil
}

func (s *configMapExpiryStore) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[name]; !ok {
		return nil
	}
	next := make(map[string]string, len(s.data)-1)
	for k, v := range s.data {
		if k != name {
			next[k] = v
		}
	}
	if err := s.persist(next); err != nil {
		return err
	}
	s.data = next
	return nil
}

// persist writes data to the ConfigMap, creating it if absent.
func (s *configMapExpiryStore) persist(data map[string]string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(ctx, s.name, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		_, err = s.clientset.CoreV1().ConfigMaps(s.namespace).Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: s.name, Namespace: s.namespace},
			Data:       data,
		}, metav1.CreateOptions{})
		return err
	}
	cm.Data = data
	_, err = s.clientset.CoreV1().ConfigMaps(s.namespace).Update(ctx, cm, metav1.UpdateOptions{})
	return err
}
